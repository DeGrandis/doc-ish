package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/ingest"
	"github.com/DeGrandis/mymedical/internal/store"
)

// Follow-ups: what files say should happen later ("Recheck blood count
// and iron studies in about six months"), filed with the file, listed as
// what's coming up, and closed when a later value or file shows it done.

// addFollowUps files a result's follow-ups for the file's owner, each due
// counted from the note that said it.
func addFollowUps(tx *sql.Tx, d store.Document, res ingest.Result) ([]int64, error) {
	open, err := store.OpenFollowUpsTx(tx, d.PersonID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, f := range open {
		have[strings.ToLower(f.What)+"|"+f.Due] = true
	}
	var ids []int64
	docID := d.ID
	for _, f := range res.FollowUps {
		from := f.From
		if from == "" {
			from = res.DocDate
		}
		if from == "" {
			from = d.Added[:10]
		}
		due := f.Due
		if due == "" {
			due = ingest.DueDate(f.When, from)
		}
		if k := strings.ToLower(f.What) + "|" + due; have[k] {
			continue // the same one, from another copy of the record
		} else {
			have[k] = true
		}
		id, err := store.AddFollowUpTx(tx, store.FollowUp{PersonID: d.PersonID, DocumentID: &docID, What: f.What, When: f.When,
			Due: due, From: from, Source: "reader"})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

var (
	imagingWords = regexp.MustCompile(`(?i)\b(ct|cat scan|mri|x-?ray|ultrasound|sonogram|mammogram|pet|dexa|bone density|echo(cardiogram)?|scan|imaging)\b`)
	vaccineWords = regexp.MustCompile(`(?i)\b(vaccin\w*|shot|booster|immuni[sz]\w*|flu|tdap|tetanus)\b`)
	labWords     = regexp.MustCompile(`(?i)\b(lab|labs|blood|level|levels|panel|count|studies|test|tests|recheck|a1c|cholesterol|lipids?|iron|ferritin|b12|vitamin|thyroid|tsh|kidney|liver|glucose|sugar|urine)\b`)
	visitWords   = regexp.MustCompile(`(?i)\b(follow[- ]?up|revisit|return|see|visit|appointment|check-?up|physical)\b`)
	wordRun      = regexp.MustCompile(`[a-z0-9]+`)
)

// What a follow-up's words point to in the values: "blood count" is
// hemoglobin and the rest of a CBC, "iron studies" iron and ferritin.
var labTerms = map[string][]string{
	"blood": {"hemoglobin", "hematocrit", "wbc", "white", "rbc", "red blood", "platelet"}, "count": {"hemoglobin", "wbc", "platelet"},
	"cbc": {"hemoglobin", "wbc", "platelet"}, "iron": {"iron", "ferritin", "tibc"}, "ferritin": {"ferritin"},
	"lipid": {"cholesterol", "ldl", "hdl", "triglyceride"}, "lipids": {"cholesterol", "ldl", "hdl", "triglyceride"},
	"cholesterol": {"cholesterol", "ldl", "hdl"}, "ldl": {"ldl"}, "a1c": {"a1c"}, "sugar": {"a1c", "glucose"}, "glucose": {"glucose", "a1c"},
	"kidney": {"creatinine", "egfr", "bun"}, "liver": {"alt", "ast", "bilirubin"}, "thyroid": {"tsh", "t4"}, "tsh": {"tsh"},
	"b12": {"b12"}, "vitamin": {"vitamin"}, "urine": {"urine", "urinalysis"}, "potassium": {"potassium"}, "sodium": {"sodium"},
}

var (
	modalities = map[string]bool{"ct": true, "mri": true, "xray": true, "ultrasound": true, "mammogram": true, "pet": true, "dexa": true, "echo": true}
	bodyParts  = map[string]bool{"chest": true, "lung": true, "head": true, "brain": true, "abdomen": true, "pelvis": true, "spine": true,
		"knee": true, "hip": true, "shoulder": true, "breast": true, "thyroid": true, "heart": true, "kidney": true, "liver": true}
)

// scanWordsOf is a scan's name as words, with "X-ray" as one and "lungs"
// as "lung".
func scanWordsOf(s string) []string {
	s = strings.NewReplacer("x-ray", "xray", "x ray", "xray", "lungs", "lung", "cat scan", "ct").Replace(strings.ToLower(s))
	return wordRun.FindAllString(s, -1)
}

// anyIn reports whether have holds one of want, or want is empty.
func anyIn(have map[string]bool, want []string) bool {
	for _, w := range want {
		if have[w] {
			return true
		}
	}
	return len(want) == 0
}

// followKind sorts a follow-up by what would show it done.
func followKind(what string) string {
	switch {
	case imagingWords.MatchString(what):
		return "imaging"
	case vaccineWords.MatchString(what):
		return "vaccine"
	case labWords.MatchString(what):
		return "lab"
	case visitWords.MatchString(what):
		return "visit"
	}
	return "other"
}

func hasAny(s string, any []string) bool {
	s = strings.ToLower(s)
	for _, w := range any {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// shownDone finds the first thing after a follow-up that shows it done:
// a lab value it asked for, a scan of the kind it asked for, a later
// visit (a visit note, or vitals read from a file), a vaccination.
func shownDone(f store.FollowUp, ev []store.Evidence) (store.Evidence, bool) {
	words := wordRun.FindAllString(strings.ToLower(f.What), -1)
	switch followKind(f.What) {
	case "imaging":
		// The kind of scan and the part of the body, when it says: a
		// chest X-ray isn't the chest CT asked for.
		var modes, parts []string
		for _, w := range scanWordsOf(f.What) {
			if modalities[w] {
				modes = append(modes, w)
			} else if bodyParts[w] {
				parts = append(parts, w)
			}
		}
		for _, e := range ev {
			if e.Kind != "imaging" {
				continue
			}
			have := map[string]bool{}
			for _, w := range scanWordsOf(e.Name) {
				have[w] = true
			}
			if anyIn(have, modes) && anyIn(have, parts) {
				return e, true
			}
		}
	case "lab":
		var want []string
		for _, w := range words {
			want = append(want, labTerms[w]...)
		}
		for _, e := range ev {
			if len(want) > 0 && e.Kind == "" && hasAny(e.Name, want) || len(want) == 0 && e.Kind == "lab" {
				return e, true
			}
		}
	case "visit":
		for _, e := range ev {
			if e.Kind == "visit" || e.Kind == "" && e.Source == "document" && hasAny(e.Name, []string{"blood pressure", "heart rate", "pulse"}) {
				return e, true
			}
		}
	case "vaccine":
		var want []string
		for _, w := range words {
			if vaccineWords.MatchString(w) && !hasAny(w, []string{"vaccin", "shot", "booster", "immuni"}) {
				want = append(want, w)
			}
		}
		for _, e := range ev {
			if (e.Kind == "vaccination" || e.Source == "vaccination") && (len(want) == 0 || hasAny(e.Name, want)) {
				return e, true
			}
		}
	}
	return store.Evidence{}, false
}

// closeFollowUps closes a person's open follow-ups that later values,
// files or vaccinations show done, and returns the ones it closed.
func closeFollowUps(tx *sql.Tx, personID int64) ([]int64, error) {
	open, err := store.OpenFollowUpsTx(tx, personID)
	if err != nil || len(open) == 0 {
		return nil, err
	}
	p, err := store.PersonTx(tx, personID)
	if err != nil {
		return nil, err
	}
	var closed []int64
	for _, f := range open {
		from := f.From
		if from == "" {
			from = f.Added[:10]
		}
		ev, err := store.EvidenceAfterTx(tx, personID, from)
		if err != nil {
			return nil, err
		}
		for _, v := range p.Profile.Vaccinations {
			if d := itemDate(v.Since); d > from {
				ev = append(ev, store.Evidence{Date: d, Name: v.Name, Source: "vaccination"})
			}
		}
		e, ok := shownDone(f, ev)
		if !ok {
			continue
		}
		f.Status, f.DoneDate, f.DoneDocumentID = store.FollowUpDone, e.Date, e.DocumentID
		f.DoneNote = fmt.Sprintf("%s on %s", e.Name, e.Date)
		if err := store.UpdateFollowUpTx(tx, f); err != nil {
			return nil, err
		}
		closed = append(closed, f.ID)
	}
	return closed, nil
}

var isoDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// itemDate is a profile entry's date, when it has a whole one.
func itemDate(since string) string {
	if m := isoDay.FindString(strings.TrimSpace(since)); m != "" {
		return m
	}
	return ""
}

func (a *App) handleFollowUps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		fs, err := st.FollowUps(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, fs)
	})
}

// newFollowUp checks and completes a follow-up someone typed or asked for.
func newFollowUp(personID int64, what, due, when, source string) (store.FollowUp, error) {
	what, due, when = strings.TrimSpace(what), strings.TrimSpace(due), strings.TrimSpace(when)
	if what == "" {
		return store.FollowUp{}, errors.New("say what should happen")
	}
	today := time.Now().Format("2006-01-02")
	if due != "" {
		if _, err := time.Parse("2006-01-02", due); err != nil {
			return store.FollowUp{}, errors.New("the due date should be YYYY-MM-DD")
		}
	} else if when != "" {
		due = ingest.DueDate(when, today)
	}
	return store.FollowUp{PersonID: personID, What: what, When: when, Due: due, From: today, Source: source}, nil
}

func (a *App) handleAddFollowUp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct{ What, Due, When string }
	if !readJSON(w, r, &in) {
		return
	}
	f, err := newFollowUp(id, in.What, in.Due, in.When, "you")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		if _, err := st.Person(id); err != nil {
			storeErr(w, err)
			return
		}
		err := st.Tx(func(tx *sql.Tx) error {
			fid, err := store.AddFollowUpTx(tx, f)
			if err != nil {
				return err
			}
			f.ID = fid
			return store.LogTx(tx, id, "you", "Added to what's coming up: "+followText(f), undoOp{Op: "followup", FollowUpIDs: []int64{fid}})
		})
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, f)
	})
}

// followText is a follow-up in a few words, for Activity.
func followText(f store.FollowUp) string {
	if f.Due != "" {
		return fmt.Sprintf("%s (due %s)", f.What, f.Due)
	}
	return f.What
}

// setFollowUpTx marks a follow-up done, set aside or open again, or
// changes its due date, and logs it with an undo.
func setFollowUpTx(tx *sql.Tx, id int64, status, due *string, actor string) (store.FollowUp, int64, error) {
	f, err := store.FollowUpTx(tx, id)
	if err != nil {
		return f, 0, err
	}
	prev := f
	var did string
	if status != nil && *status != f.Status {
		today := time.Now().Format("2006-01-02")
		switch *status {
		case store.FollowUpDone:
			f.DoneDate, f.DoneNote, f.DoneDocumentID = today, "Marked done", nil
			did = "Done: "
		case store.FollowUpDismissed:
			f.DoneDate, f.DoneNote, f.DoneDocumentID = today, "", nil
			did = "Set aside: "
		case store.FollowUpOpen:
			f.DoneDate, f.DoneNote, f.DoneDocumentID = "", "", nil
			did = "Open again: "
		default:
			return f, 0, errors.New("status is open, done or dismissed")
		}
		f.Status = *status
	}
	if due != nil && *due != f.Due {
		if *due != "" {
			if _, err := time.Parse("2006-01-02", *due); err != nil {
				return f, 0, errors.New("the due date should be YYYY-MM-DD")
			}
		}
		f.Due = *due
		if did == "" {
			did = "New due date: "
		}
	}
	if did == "" {
		return f, 0, nil
	}
	if err := store.UpdateFollowUpTx(tx, f); err != nil {
		return f, 0, err
	}
	aid, err := store.LogIDTx(tx, f.PersonID, 0, actor, did+followText(f), undoOp{Op: "followup", PrevFollowUp: &prev})
	return f, aid, err
}

func (a *App) handleUpdateFollowUp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Status *string `json:"status"`
		Due    *string `json:"due"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		var f store.FollowUp
		err := st.Tx(func(tx *sql.Tx) error {
			var err error
			f, _, err = setFollowUpTx(tx, id, in.Status, in.Due, "you")
			return err
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				storeErr(w, err)
				return
			}
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, f)
	})
}

// followLine is a follow-up for the assistant's context.
func followLine(f store.FollowUp, today string) string {
	s := fmt.Sprintf("[f:%d] %s", f.ID, f.What)
	switch {
	case f.Due == "":
		if f.When != "" {
			s += " (" + f.When + ")"
		}
	case f.Due < today:
		s += " — was due " + f.Due + ", OVERDUE"
	default:
		s += " — due " + f.Due
	}
	if f.From != "" && f.Source == "reader" {
		s += ", from a note of " + f.From
	}
	if f.DocumentID != nil {
		s += fmt.Sprintf(" [doc:%d]", *f.DocumentID)
	}
	return s
}

func (e *toolEnv) addFollowUp(what, due, when string) (string, error) {
	f, err := newFollowUp(e.pid, what, due, when, "assistant")
	if err != nil {
		return "", err
	}
	summary := "Added to what's coming up: " + followText(f)
	var aid int64
	err = e.st.Tx(func(tx *sql.Tx) error {
		id, err := store.AddFollowUpTx(tx, f)
		if err != nil {
			return err
		}
		aid, err = store.LogIDTx(tx, e.pid, 0, "assistant", summary, undoOp{Op: "followup", FollowUpIDs: []int64{id}})
		return err
	})
	if err != nil {
		return "", err
	}
	e.changed(aid, summary)
	return "Saved. " + summary + ". The user sees it with an Undo button.", nil
}

func (e *toolEnv) setFollowUp(id int64, status, due string) (string, error) {
	var st, d *string
	if status != "" {
		st = &status
	}
	if due != "" {
		d = &due
	}
	var f store.FollowUp
	var aid int64
	err := e.st.Tx(func(tx *sql.Tx) error {
		cur, err := store.FollowUpTx(tx, id)
		if err != nil {
			return err
		}
		if cur.PersonID != e.pid {
			return errors.New("that follow-up is someone else's")
		}
		f, aid, err = setFollowUpTx(tx, id, st, d, "assistant")
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("there's no follow-up %d", id)
		}
		return "", err
	}
	if aid == 0 {
		return "Nothing to change: " + followText(f) + " is already " + f.Status + ".", nil
	}
	summary := map[string]string{store.FollowUpDone: "Done: ", store.FollowUpDismissed: "Set aside: ", store.FollowUpOpen: "Open: "}[f.Status] + followText(f)
	e.changed(aid, summary)
	return "Saved. " + summary + ". The user sees it with an Undo button.", nil
}
