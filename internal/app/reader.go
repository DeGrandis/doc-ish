package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/ingest"
	"github.com/DeGrandis/mymedical/internal/openai"
	"github.com/DeGrandis/mymedical/internal/recipes"
	"github.com/DeGrandis/mymedical/internal/store"
)

// The reader works through files waiting to be read, one at a time (one
// GPU), in the background. A file it's sure about is filed; anything else
// waits in Review.

const (
	readTimeout = 6 * time.Minute
	maxTextKept = 1 << 20 // characters of extracted text kept for search and the assistant
)

// wakeReader nudges the reader to look for work now.
func (a *App) wakeReader() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *App) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-time.After(time.Minute):
		}
		for ctx.Err() == nil && a.readNext(ctx) {
		}
	}
}

// readerState is what Settings and the file lists show about the reader.
type readerState struct {
	Reading int64  `json:"reading,omitempty"` // the document being read
	Error   string `json:"error,omitempty"`   // why it's paused
}

func (a *App) setReader(id int64, errMsg string) {
	a.readerMu.Lock()
	a.reader = readerState{Reading: id, Error: errMsg}
	a.readerMu.Unlock()
}

func (a *App) readerStatus() readerState {
	a.readerMu.Lock()
	defer a.readerMu.Unlock()
	return a.reader
}

// transient reports whether an error means "try later" (Ollama is down or
// busy) rather than "this file can't be read".
func transient(err error) bool {
	m := err.Error()
	return strings.HasPrefix(m, "couldn't reach Ollama") || strings.HasPrefix(m, "Ollama didn't answer") ||
		strings.HasPrefix(m, "can't reach the provider") || strings.HasPrefix(m, "the provider didn't answer") ||
		openai.Limited(err) || errors.Is(err, context.DeadlineExceeded)
}

// readNext reads one waiting file. It reports whether to go on at once.
func (a *App) readNext(ctx context.Context) bool {
	var (
		doc    store.Document
		set    Settings
		owner  ingest.Person
		others []ingest.Person
		cont   ingest.Content
		exErr  error
	)
	a.mu.RLock()
	if a.st == nil {
		a.mu.RUnlock()
		return false
	}
	st, files := a.st, a.files
	set = a.settings(st)
	if msg := set.ready(); msg != "" {
		a.mu.RUnlock()
		a.setReader(0, "To read files, "+msg+".")
		return false
	}
	var err error
	if doc, err = st.NextToRead(); err != nil {
		a.mu.RUnlock()
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("reader: %v", err)
		}
		a.setReader(0, "")
		return false
	}
	st.SetStatus(doc.ID, store.StatusReading, "")
	a.setReader(doc.ID, "")
	people, _ := st.People()
	for _, p := range people {
		ip := ingest.Person{ID: p.ID, Name: p.Name, Born: p.Profile.Born}
		if p.ID == doc.PersonID {
			owner = ip
		} else {
			others = append(others, ip)
		}
	}
	// Extract while holding the lock, so Lock waits for it instead of the
	// file being read after the key is meant to be forgotten.
	if rc, err := files.Open(doc.Blob); err != nil {
		exErr = fmt.Errorf("couldn't open the stored file: %w", err)
	} else {
		cont, exErr = ingest.Extract(ctx, doc.Mime, doc.Filename, rc, tmpDir())
		rc.Close()
	}
	// A format someone approved a recipe for is read by the recipe alone.
	var (
		fingerprint string
		recipe      *store.Recipe
	)
	if exErr == nil && cont.Full != "" {
		if fingerprint = recipes.Fingerprint(cont.Full); fingerprint != "" {
			if r, err := st.ApprovedRecipe(fingerprint); err == nil {
				recipe = &r
			}
		}
	}
	a.mu.RUnlock()

	var recipeNote string
	if recipe != nil {
		recs, problems, err := runRecipe(ctx, recipe.Script, cont.Full)
		if err == nil && len(recs) > 0 {
			return a.fileByRecipe(doc.ID, *recipe, recs, problems)
		}
		if err == nil {
			err = errors.New("it found nothing")
		}
		recipeNote = fmt.Sprintf("The import recipe “%s” didn't work on this file (%v), so the reader looked at it instead.", recipe.Name, err)
	}

	var res ingest.Result
	if exErr == nil {
		c, err := set.chatter()
		if err != nil {
			exErr = err
		} else {
			rctx, cancel := context.WithTimeout(ctx, readTimeout)
			res, exErr = ingest.Read(rctx, c, ingest.Request{
				Filename: doc.Filename, PersonName: owner.Name, Today: time.Now(), Content: cont,
				ChatModel: set.chatModel(), VisionModel: set.visionModel(), NumCtx: set.NumCtx,
				// A long table: an import recipe reads the values.
				DescribeOnly: cont.Sampled && fingerprint != "",
			})
			cancel()
		}
	}

	// A table too long to copy one value at a time: ask for a recipe that
	// reads all of it, for a person to check.
	var draft *recipes.Draft
	var draftErr error
	if exErr == nil && recipe == nil && cont.Sampled && fingerprint != "" {
		if c, err := set.chatter(); err == nil {
			wctx, cancel := context.WithTimeout(ctx, readTimeout)
			d, err := recipes.Write(wctx, c, recipes.WriteRequest{
				Model: set.chatModel(), NumCtx: set.NumCtx, Filename: doc.Filename, Text: cont.Full, Today: time.Now(),
			})
			cancel()
			if err == nil {
				draft = &d
			} else {
				draftErr = err
			}
		}
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.st == nil {
		return false // locked meanwhile; it goes back in line at the next unlock
	}
	st = a.st
	cur, err := st.Document(doc.ID)
	if err != nil || cur.Status != store.StatusReading {
		return true // deleted, or someone changed it, while it was being read
	}
	if exErr != nil {
		if ctx.Err() != nil || transient(exErr) {
			st.SetStatus(doc.ID, store.StatusNew, "")
			a.setReader(0, exErr.Error())
			return false
		}
		st.SetStatus(doc.ID, store.StatusFailed, exErr.Error())
		st.LogDoc(cur.PersonID, cur.ID, "reader", fmt.Sprintf("Couldn't read %s: %s", cur.Filename, exErr.Error()), nil)
		a.setReader(0, "")
		return true
	}
	reasons, _ := ingest.Check(res, cont, owner, others, time.Now())
	if recipeNote != "" {
		reasons = append(reasons, recipeNote)
	}
	switch {
	case draft != nil:
		reasons = append(reasons, fmt.Sprintf("The reader wrote an import recipe that reads all of it: %d values. Check it below.", len(draft.Records)))
	case draftErr != nil:
		reasons = append(reasons, "It tried to write an import recipe for the whole table but couldn't: "+draftErr.Error())
	}
	if err := a.file(st, cur, res, cont, reasons); err != nil {
		log.Printf("reader: save %d: %v", doc.ID, err)
		st.SetStatus(doc.ID, store.StatusFailed, "couldn't save what was read")
	} else if draft != nil {
		a.propose(st, cur.ID, fingerprint, *draft)
	} else if res.Kind == "imaging" {
		linkReports(st, cur.PersonID) // a radiology report finds its images
	}
	a.setReader(0, "")
	return true
}

// runRecipe runs an approved or draft recipe on a whole file.
func runRecipe(ctx context.Context, script, text string) ([]recipes.Record, []string, error) {
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := recipes.Run(rctx, script, text, recipes.DefaultLimits)
	if err != nil {
		return nil, nil, err
	}
	recs, problems := recipes.Validate(out.Records, time.Now())
	return recs, problems, nil
}

// propose saves a draft recipe for a file, with a preview of what it found.
func (a *App) propose(st *store.Store, docID int64, fingerprint string, d recipes.Draft) {
	preview, _ := json.Marshal(recipes.PreviewOf(d.Records, d.Problems))
	if _, err := st.ProposeRecipe(store.Recipe{
		Name: d.Name, Kind: d.Kind, Description: d.Description, Fingerprint: fingerprint, Script: d.Script,
		DocumentID: &docID, Preview: preview,
	}); err != nil {
		log.Printf("reader: save draft recipe: %v", err)
	}
}

// fileByRecipe files every value a recipe found in a file, replacing
// anything read from it before.
func (a *App) fileByRecipe(docID int64, r store.Recipe, recs []recipes.Record, problems []string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.st == nil {
		return false
	}
	st := a.st
	d, err := st.Document(docID)
	if err != nil || d.Status != store.StatusReading {
		return true
	}
	prev := snapshot(d)
	prev.Status = store.StatusDone
	err = st.Tx(func(tx *sql.Tx) error {
		_, err := fileRecordsTx(tx, &d, r, recs, problems)
		if err != nil {
			return err
		}
		return store.LogDocTx(tx, d.PersonID, d.ID, "reader",
			fmt.Sprintf("Read %s with the recipe “%s”: %d values", d.Filename, r.Name, len(recs)),
			undoOp{Op: "read", DocumentID: d.ID, Prev: &prev, AllValues: true})
	})
	if err != nil {
		log.Printf("reader: file %d by recipe: %v", docID, err)
		st.SetStatus(docID, store.StatusFailed, "couldn't save what the recipe found")
	}
	a.setReader(0, "")
	return true
}

// fileRecordsTx replaces a file's values with a recipe's and describes
// the file from them.
func fileRecordsTx(tx *sql.Tx, d *store.Document, r store.Recipe, recs []recipes.Record, problems []string) (int, error) {
	old, err := store.DocumentObservationIDsTx(tx, d.ID)
	if err != nil {
		return 0, err
	}
	if err := store.DeleteObservationsTx(tx, old); err != nil {
		return 0, err
	}
	docID := d.ID
	for _, rec := range recs {
		if _, err := store.AddObservationTx(tx, store.Observation{
			PersonID: d.PersonID, Name: rec.Name, Value: rec.Value, ValueText: rec.Text, Unit: rec.Unit,
			RefLow: rec.RefLow, RefHigh: rec.RefHigh, Flag: rec.Flag, Observed: rec.Date, DocumentID: &docID,
			Source: "recipe", Note: rec.Note,
		}); err != nil {
			return 0, err
		}
	}
	series := recipes.Summarize(recs)
	var names []string
	first, last := "", ""
	for i, s := range series {
		if i < 4 {
			names = append(names, s.Name)
		}
		if first == "" || s.First < first {
			first = s.First
		}
		if s.Last > last {
			last = s.Last
		}
	}
	d.Kind = r.Kind
	if d.Kind == "" {
		d.Kind = "device"
	}
	d.Title = r.Name
	if last != "" {
		d.DocDate = last[:10]
	}
	d.Summary = fmt.Sprintf("%d readings of %s from %s to %s, read with the import recipe “%s”.",
		len(recs), strings.Join(names, ", "), first[:min(len(first), 10)], last[:min(len(last), 10)], r.Name)
	if len(problems) > 0 {
		d.Summary += fmt.Sprintf(" %d rows were skipped.", len(problems))
	}
	d.Status, d.ReviewReason, d.Extracted = store.StatusDone, "", ""
	if err := store.UpdateDocumentTx(tx, *d); err != nil {
		return 0, err
	}
	if err := store.SetDocumentRecipeTx(tx, d.ID, r.ID); err != nil {
		return 0, err
	}
	return len(recs), store.UsedRecipeTx(tx, r.ID)
}

func tmpDir() string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		return "/dev/shm" // memory, so plain copies never touch the disk
	}
	return os.TempDir()
}

// docSnapshot is what undoing a read or an edit puts back.
type docSnapshot struct {
	PersonID     int64  `json:"person_id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	DocDate      string `json:"doc_date"`
	Provider     string `json:"provider"`
	Summary      string `json:"summary"`
	Status       string `json:"status"`
	ReviewReason string `json:"review_reason"`
}

func snapshot(d store.Document) docSnapshot {
	return docSnapshot{d.PersonID, d.Kind, d.Title, d.DocDate, d.Provider, d.Summary, d.Status, d.ReviewReason}
}

func (s docSnapshot) restore(d *store.Document) {
	d.PersonID, d.Kind, d.Title, d.DocDate, d.Provider, d.Summary, d.Status, d.ReviewReason =
		s.PersonID, s.Kind, s.Title, s.DocDate, s.Provider, s.Summary, s.Status, s.ReviewReason
}

// undoOp is the undo record kept with each change.
type undoOp struct {
	Op             string           `json:"op"` // read, accept, document, profile, observation
	DocumentID     int64            `json:"document_id,omitempty"`
	Prev           *docSnapshot     `json:"prev,omitempty"`
	ObservationIDs []int64          `json:"observation_ids,omitempty"`
	PersonID       int64            `json:"person_id,omitempty"`
	PrevName       string           `json:"prev_name,omitempty"`
	PrevProfile    *json.RawMessage `json:"prev_profile,omitempty"`
	AllValues      bool             `json:"all_values,omitempty"` // remove every value read from the file (too many to list)
	RecipeID       int64            `json:"recipe_id,omitempty"`  // a recipe approved by this change
}

// file saves what was read: filed with its values when nothing needs a
// look, otherwise held in Review with the reasons and no values yet.
func (a *App) file(st *store.Store, d store.Document, res ingest.Result, cont ingest.Content, reasons []string) error {
	prev := snapshot(d)
	prev.Status = store.StatusDone // undoing a read leaves the file stored, not re-read

	raw, _ := json.Marshal(res)
	d.Extracted = string(raw)
	d.Kind, d.DocDate, d.Provider, d.Summary = res.Kind, res.DocDate, res.Provider, res.Summary
	if res.Title != "" {
		d.Title = res.Title
	}
	text := cont.Text
	if len(text) > maxTextKept {
		text = strings.ToValidUTF8(text[:maxTextKept], "")
	}
	d.Text = text
	if len(reasons) > 0 {
		d.Status, d.ReviewReason = store.StatusReview, strings.Join(reasons, "\n")
	} else {
		d.Status, d.ReviewReason = store.StatusDone, ""
	}
	return st.Tx(func(tx *sql.Tx) error {
		if err := store.UpdateDocumentTx(tx, d); err != nil {
			return err
		}
		var ids []int64
		if d.Status == store.StatusDone {
			var err error
			if ids, err = addValues(tx, d, res); err != nil {
				return err
			}
		}
		what := d.Title
		if what == "" {
			what = d.Kind
		}
		summary := fmt.Sprintf("Read %s: %s", d.Filename, what)
		if len(ids) > 0 {
			summary += fmt.Sprintf(", %d value%s", len(ids), plural(len(ids)))
		}
		if d.Status == store.StatusReview {
			summary += ". Needs a look"
		}
		return store.LogDocTx(tx, d.PersonID, d.ID, "reader", summary, undoOp{Op: "read", DocumentID: d.ID, Prev: &prev, ObservationIDs: ids})
	})
}

// addValues stores a result's values as observations of the file's owner.
func addValues(tx *sql.Tx, d store.Document, res ingest.Result) ([]int64, error) {
	var ids []int64
	docID := d.ID
	for _, v := range res.Observations {
		when := v.Date
		if when == "" {
			when = res.DocDate
		}
		if when == "" {
			when = d.Added[:10]
		}
		id, err := store.AddObservationTx(tx, store.Observation{
			PersonID: d.PersonID, Name: v.Name, Value: v.Value, ValueText: v.ValueText, Unit: v.Unit,
			RefLow: v.RefLow, RefHigh: v.RefHigh, Flag: v.Flag, Observed: when, DocumentID: &docID, Source: "document",
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
