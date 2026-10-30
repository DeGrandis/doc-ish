package app

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/healthexport"
	"github.com/DeGrandis/mymedical/internal/ingest"
	"github.com/DeGrandis/mymedical/internal/store"
)

// Health exports (Apple Health, Google Fit) are read by fixed code, not
// the model: the formats are known and can hold millions of readings. The
// whole export is kept as one file, and everything imported from it hangs
// off that file, so one Undo takes it all back out.

const (
	mimeExportZip = "application/x-health-export+zip"
	mimeExportXML = "application/x-health-export+xml"
	exportSource  = "device"
	exportChecked = `{"checked":true}` // someone accepted it in Review; import without the checks
	importTimeout = 30 * time.Minute
)

func isExport(mime string) bool { return mime == mimeExportZip || mime == mimeExportXML }

// exportKind says whether a zip on disk is a health export ("apple",
// "google") or not ("").
func exportKind(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	return healthexport.Kind(&zr.Reader)
}

// addExport stores an export zip whole, to be imported by the reader.
func (a *App) addExport(st *store.Store, files *blobs.Store, pid int64, path, name string) uploadResult {
	res := uploadResult{Filename: name}
	f, err := os.Open(path)
	if err != nil {
		res.Error = "couldn't take the export in"
		return res
	}
	defer f.Close()
	blob, size, sum, err := files.Put(f)
	if err != nil {
		res.Error = "couldn't store it"
		return res
	}
	d, err := st.AddDocument(store.Document{PersonID: pid, Blob: blob, Filename: name, Mime: mimeExportZip, Size: size, SHA256: sum})
	switch {
	case errors.Is(err, store.ErrDuplicate):
		files.Delete(blob)
		res.Duplicate, res.Document = true, &d
	case err != nil:
		files.Delete(blob)
		res.Error = "couldn't save it"
	default:
		res.Document = &d
	}
	return res
}

// importExport reads a stored export and files what's in it, or holds it
// in Review when it doesn't look like the profile's person.
func (a *App) importExport(ctx context.Context, doc store.Document) bool {
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.st == nil {
		return false
	}
	st, files := a.st, a.files
	st.SetStatus(doc.ID, store.StatusReading, "")
	a.setReader(doc.ID, "")
	defer a.setReader(0, "")
	fail := func(msg string) bool {
		st.SetStatus(doc.ID, store.StatusFailed, msg)
		st.LogDoc(doc.PersonID, doc.ID, "reader", fmt.Sprintf("Couldn't import %s: %s", doc.Filename, msg), nil)
		return true
	}
	rc, err := files.Open(doc.Blob)
	if err != nil {
		return fail("couldn't open the stored file")
	}
	var ex *healthexport.Export
	if doc.Mime == mimeExportXML {
		ex, err = healthexport.ParseAppleXML(ctx, rc)
		rc.Close()
	} else {
		// A zip needs to be read out of order, so it goes to a plain
		// temporary copy, removed straight after.
		path, serr := spool(rc, filepath.Join(a.cfg.DataDir, "tmp"))
		rc.Close()
		if serr != nil {
			return fail("couldn't unpack it")
		}
		defer os.Remove(path)
		zr, zerr := zip.OpenReader(path)
		if zerr != nil {
			return fail("the zip is damaged")
		}
		ex, err = healthexport.ParseZip(ctx, &zr.Reader)
		zr.Close()
	}
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			st.SetStatus(doc.ID, store.StatusNew, "") // the app is stopping; try again next time
			return false
		}
		return fail(err.Error())
	}
	cur, err := st.Document(doc.ID)
	if err != nil || cur.Status != store.StatusReading {
		return true // deleted or changed meanwhile
	}
	if err := fileExport(st, cur, ex); err != nil {
		log.Printf("import %d: %v", doc.ID, err)
		return fail("couldn't save what was in it")
	}
	return true
}

// exportReasons are why an export needs a look before it's filed: a birth
// date or a name in it that isn't the profile's.
func exportReasons(st *store.Store, p store.Person, ex *healthexport.Export) []string {
	var reasons []string
	if ex.Born != "" && p.Profile.Born != "" && ex.Born != p.Profile.Born {
		reasons = append(reasons, fmt.Sprintf("The export gives a birth date of %s, but %s's profile says %s.", ex.Born, p.Name, p.Profile.Born))
	}
	people, _ := st.People()
	seen := map[string]bool{}
	for _, n := range ex.Names {
		if seen[n] || ingest.NameMatches(p.Name, n) {
			continue
		}
		seen[n] = true
		r := fmt.Sprintf("Records in it are for “%s”, which doesn't look like %s.", n, p.Name)
		for _, o := range people {
			if o.ID != p.ID && ingest.NameMatches(o.Name, n) {
				r = fmt.Sprintf("Records in it are for “%s”. They look like %s's, not %s's.", n, o.Name, p.Name)
				break
			}
		}
		reasons = append(reasons, r)
	}
	return reasons
}

func fileExport(st *store.Store, d store.Document, ex *healthexport.Export) error {
	p, err := st.Person(d.PersonID)
	if err != nil {
		return err
	}
	prev := snapshot(d)
	prev.Status = store.StatusDone
	d.Kind, d.Title, d.Provider = "device", ex.Source+" export", ex.Source
	d.DocDate = ex.Exported
	if d.DocDate == "" {
		for _, r := range ex.Readings {
			if !r.FromChart && r.When > d.DocDate {
				d.DocDate = r.When[:10]
			}
		}
	}
	d.Summary = healthexport.Summary(ex)
	d.Text = healthexport.Overview(ex)
	if len(d.Text) > maxTextKept {
		d.Text = strings.ToValidUTF8(d.Text[:maxTextKept], "")
	}
	var reasons []string
	if d.Extracted != exportChecked {
		reasons = exportReasons(st, p, ex)
	}
	d.Extracted = ""
	if len(reasons) > 0 {
		d.Status, d.ReviewReason = store.StatusReview, strings.Join(reasons, "\n")
		return st.Tx(func(tx *sql.Tx) error {
			if err := store.UpdateDocumentTx(tx, d); err != nil {
				return err
			}
			return store.LogDocTx(tx, d.PersonID, d.ID, "reader", fmt.Sprintf("Read %s: %s. Needs a look", d.Filename, d.Title),
				undoOp{Op: "read", DocumentID: d.ID, Prev: &prev})
		})
	}
	d.Status, d.ReviewReason = store.StatusDone, ""
	return st.Tx(func(tx *sql.Tx) error {
		added, replaced, err := addExportValues(tx, d, ex)
		if err != nil {
			return err
		}
		prevProfile, _ := json.Marshal(p.Profile)
		raw := json.RawMessage(prevProfile)
		changes := mergeExportProfile(&p.Profile, ex)
		if len(changes) > 0 {
			if err := store.UpdatePersonTx(tx, p); err != nil {
				return err
			}
			d.Summary += " Added to the profile: " + strings.Join(changes, "; ") + "."
		}
		if len(ex.Skipped) > 0 {
			d.Summary += " Not imported: " + skippedList(ex.Skipped) + "."
		}
		if err := store.UpdateDocumentTx(tx, d); err != nil {
			return err
		}
		summary := fmt.Sprintf("Imported %s: %d value%s", d.Filename, added, plural(added))
		if skipped := len(ex.Readings) - added - replaced; skipped > 0 {
			summary += fmt.Sprintf(" (%d already on record from an earlier export)", skipped)
		}
		if len(changes) > 0 {
			summary += "; added to the profile: " + strings.Join(changes, "; ")
		}
		closed, err := closeFollowUps(tx, d.PersonID) // hospital labs may be the recheck someone was waiting for
		if err != nil {
			return err
		}
		u := undoOp{Op: "read", DocumentID: d.ID, Prev: &prev, AllValues: true, ReopenIDs: closed}
		if len(changes) > 0 {
			u.PersonID, u.PrevName, u.PrevProfile = p.ID, p.Name, &raw
		}
		return store.LogDocTx(tx, d.PersonID, d.ID, "reader", summary, u)
	})
}

func skippedList(sk map[string]int) string {
	var ks []string
	for k := range sk {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return sk[ks[i]] > sk[ks[j]] })
	var out []string
	for i, k := range ks {
		if i == 8 {
			out = append(out, fmt.Sprintf("and %d more kinds", len(ks)-8))
			break
		}
		out = append(out, fmt.Sprintf("%s (%d)", k, sk[k]))
	}
	return strings.Join(out, ", ")
}

// addExportValues stores the readings, skipping any an earlier export
// already brought in: exports hold the whole history every time, so next
// month's adds only what's new. A day's total for the last day an earlier
// export had is replaced, since that export was made part way through it.
func addExportValues(tx *sql.Tx, d store.Document, ex *healthexport.Export) (added, replaced int, err error) {
	old, err := store.SourceObservationsTx(tx, d.PersonID, exportSource)
	if err != nil {
		return 0, 0, err
	}
	key := func(name, when string, v *float64, text string) string {
		if v != nil {
			return fmt.Sprintf("%s|%s|%g", strings.ToLower(name), when, *v)
		}
		return strings.ToLower(name) + "|" + when + "|" + text
	}
	have := map[string]bool{}
	days := map[string]store.Observation{} // name|day -> a day's value
	last := map[string]string{}            // name -> its last day
	for _, o := range old {
		have[key(o.Name, o.Observed, o.Value, o.ValueText)] = true
		if len(o.Observed) == 10 {
			n := strings.ToLower(o.Name)
			days[n+"|"+o.Observed] = o
			last[n] = max(last[n], o.Observed)
		}
	}
	docID := d.ID
	var obs []store.Observation
	var drop []int64
	for _, r := range ex.Readings {
		if have[key(r.Name, r.When, r.Value, r.Text)] {
			continue
		}
		n := strings.ToLower(r.Name)
		if prior, ok := days[n+"|"+r.When]; ok && r.Daily {
			if r.When != last[n] {
				continue
			}
			drop = append(drop, prior.ID)
			replaced++
		}
		note := r.Note
		if r.FromChart {
			note = strings.TrimSpace("from hospital records. " + note)
		}
		obs = append(obs, store.Observation{PersonID: d.PersonID, Name: r.Name, Code: r.Code, Value: r.Value, ValueText: r.Text,
			Unit: r.Unit, RefLow: r.RefLow, RefHigh: r.RefHigh, Flag: r.Flag, Observed: r.When, DocumentID: &docID,
			Source: exportSource, Note: note})
	}
	if err := store.DeleteObservationsTx(tx, drop); err != nil {
		return 0, 0, err
	}
	if err := store.AddObservationsTx(tx, obs); err != nil {
		return 0, 0, err
	}
	return len(obs) - replaced, replaced, nil
}

// mergeExportProfile adds what the export knows to the profile without
// overwriting anything a person entered, and says what it added.
func mergeExportProfile(pr *store.Profile, ex *healthexport.Export) []string {
	var changes []string
	if pr.Born == "" && ex.Born != "" {
		pr.Born = ex.Born
		changes = append(changes, "born "+ex.Born)
	}
	if pr.Sex == "" && ex.Sex != "" {
		pr.Sex = normalizeSex(ex.Sex)
		changes = append(changes, "sex")
	}
	if pr.BloodType == "" && ex.BloodType != "" {
		pr.BloodType = normalizeBloodType(ex.BloodType)
		changes = append(changes, "blood type "+pr.BloodType)
	}
	merge := func(list *[]store.Item, items []healthexport.Item, what string) {
		var names []string
		for _, it := range items {
			if what == "vaccinations" && hasShot(*list, it) || what != "vaccinations" && hasItem(*list, it.Name) {
				continue
			}
			*list = append(*list, store.Item{Name: it.Name, Detail: it.Detail, Since: it.Since})
			names = append(names, it.Name)
		}
		if len(names) > 0 {
			changes = append(changes, what+": "+strings.Join(names, ", "))
		}
	}
	merge(&pr.Allergies, ex.Allergies, "allergies")
	merge(&pr.Conditions, ex.Conditions, "conditions")
	merge(&pr.Medications, ex.Medications, "medications")
	merge(&pr.Vaccinations, ex.Vaccinations, "vaccinations")
	return changes
}

// hasShot reports whether a vaccination is already listed: the same
// vaccine on the same date. A yearly flu shot is a new entry each year.
func hasShot(list []store.Item, it healthexport.Item) bool {
	for _, l := range list {
		if strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(it.Name)) && (l.Since == it.Since || l.Since == "") {
			return true
		}
	}
	return false
}

// hasItem reports whether a list already has something by that name
// ("Lisinopril" is in "Lisinopril 10 mg", and the other way round).
func hasItem(list []store.Item, name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, it := range list {
		l := strings.ToLower(strings.TrimSpace(it.Name))
		if l == n || (len(l) >= 4 && strings.HasPrefix(n, l)) || (len(n) >= 4 && strings.HasPrefix(l, n)) {
			return true
		}
	}
	return false
}
