package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/ingest"
	"github.com/DeGrandis/mymedical/internal/store"
)

// docView is a file with what the reader found and the values filed from it.
type docView struct {
	store.Document
	Result     *json.RawMessage    `json:"result,omitempty"`
	Values     []store.Observation `json:"values"`
	ValueCount int                 `json:"value_count"`
	Recipe     *store.Recipe       `json:"recipe,omitempty"` // a draft waiting for approval, or the recipe that read it
}

// maxValuesShown caps the values sent with a file: a device export can
// have tens of thousands; the chat and charts get them all.
const maxValuesShown = 300

func viewOf(st *store.Store, d store.Document) docView {
	v := docView{Document: d, Values: []store.Observation{}}
	if d.Extracted != "" {
		raw := json.RawMessage(d.Extracted)
		v.Result = &raw
	}
	if vals, err := st.DocumentObservations(d.ID); err == nil {
		v.ValueCount = len(vals)
		if len(vals) > maxValuesShown {
			vals = vals[:maxValuesShown]
		}
		v.Values = vals
	}
	if d.Status == store.StatusReview {
		if r, err := st.ProposedRecipe(d.ID); err == nil {
			v.Recipe = &r
		}
	} else if d.RecipeID != nil {
		if r, err := st.Recipe(*d.RecipeID); err == nil {
			r.Preview = nil
			v.Recipe = &r
		}
	}
	return v
}

// handleReview lists a profile's files that need a look or couldn't be read.
func (a *App) handleReview(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		var out []docView
		for _, status := range []string{store.StatusReview, store.StatusFailed} {
			ds, err := st.Waiting(status)
			if err != nil {
				storeErr(w, err)
				return
			}
			for _, d := range ds {
				if d.PersonID == pid {
					out = append(out, viewOf(st, d))
				}
			}
		}
		if out == nil {
			out = []docView{}
		}
		writeJSON(w, out)
	})
}

// handleStatus is what the UI polls while files are being read: counts
// per profile and what the reader is doing.
func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		counts, err := st.Counts()
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"counts": counts, "reader": a.readerStatus(), "build": a.build})
	})
}

// handleAccept files what the reader found in a file waiting in Review.
func (a *App) handleAccept(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		err := st.Tx(func(tx *sql.Tx) error {
			d, err := store.DocumentTx(tx, id)
			if err != nil {
				return err
			}
			if d.Status != store.StatusReview {
				return errors.New("this file isn't waiting for a look")
			}
			var res ingest.Result
			if d.Extracted != "" {
				if err := json.Unmarshal([]byte(d.Extracted), &res); err != nil {
					return err
				}
			}
			prev := snapshot(d)
			d.Status, d.ReviewReason = store.StatusDone, ""
			if err := store.UpdateDocumentTx(tx, d); err != nil {
				return err
			}
			// Values already filed (say, from an earlier accept that was
			// undone and redone) are replaced, not doubled.
			old, err := store.DocumentObservationIDsTx(tx, id)
			if err != nil {
				return err
			}
			if err := store.DeleteObservationsTx(tx, old); err != nil {
				return err
			}
			ids, err := addValues(tx, d, res)
			if err != nil {
				return err
			}
			what := d.Title
			if what == "" {
				what = d.Filename
			}
			return store.LogDocTx(tx, d.PersonID, d.ID, "you", fmt.Sprintf("Checked %s: filed %d value%s", what, len(ids), plural(len(ids))),
				undoOp{Op: "accept", DocumentID: id, Prev: &prev, ObservationIDs: ids})
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				storeErr(w, err)
				return
			}
			writeErr(w, 400, err.Error())
			return
		}
		d, _ := st.Document(id)
		writeJSON(w, viewOf(st, d))
	})
}

// handleReread clears what was read from a file and puts it back in line.
func (a *App) handleReread(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		err := st.Tx(func(tx *sql.Tx) error {
			d, err := store.DocumentTx(tx, id)
			if err != nil {
				return err
			}
			if d.Status == store.StatusReading {
				return errors.New("it's being read right now")
			}
			old, err := store.DocumentObservationIDsTx(tx, id)
			if err != nil {
				return err
			}
			if err := store.DeleteObservationsTx(tx, old); err != nil {
				return err
			}
			d.Status, d.ReviewReason, d.Extracted = store.StatusNew, "", ""
			if err := store.UpdateDocumentTx(tx, d); err != nil {
				return err
			}
			return store.LogDocTx(tx, d.PersonID, d.ID, "you", "Asked to read "+d.Filename+" again", nil)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				storeErr(w, err)
				return
			}
			writeErr(w, 400, err.Error())
			return
		}
		a.wakeReader()
		writeJSON(w, map[string]bool{"ok": true})
	})
}

func (a *App) handleActivity(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		list, err := st.ActivityFor(pid, 0)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, list)
	})
}

// handleUndo reverses one change.
func (a *App) handleUndo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		err := st.Tx(func(tx *sql.Tx) error {
			act, err := store.ActivityTx(tx, id)
			if err != nil {
				return err
			}
			if !act.CanUndo {
				return errors.New("this change can't be undone")
			}
			var u undoOp
			if err := json.Unmarshal(act.Undo, &u); err != nil {
				return err
			}
			if err := applyUndo(tx, u); err != nil {
				return err
			}
			if err := store.MarkUndoneTx(tx, id); err != nil {
				return err
			}
			pid := int64(0)
			if act.PersonID != nil {
				pid = *act.PersonID
			}
			return store.LogTx(tx, pid, "you", "Undid: "+act.Summary, nil)
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, 409, "what this changed is gone")
				return
			}
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

func applyUndo(tx *sql.Tx, u undoOp) error {
	if err := store.DeleteObservationsTx(tx, u.ObservationIDs); err != nil {
		return err
	}
	if u.AllValues {
		ids, err := store.DocumentObservationIDsTx(tx, u.DocumentID)
		if err != nil {
			return err
		}
		if err := store.DeleteObservationsTx(tx, ids); err != nil {
			return err
		}
		if err := store.SetDocumentRecipeTx(tx, u.DocumentID, 0); err != nil {
			return err
		}
	}
	if u.RecipeID != 0 {
		if err := store.SetRecipeStatusTx(tx, u.RecipeID, store.RecipeProposed); err != nil {
			return err
		}
	}
	switch u.Op {
	case "observation":
		return nil
	case "read", "accept", "document", "recipe":
		d, err := store.DocumentTx(tx, u.DocumentID)
		if err != nil {
			return err
		}
		if u.Prev != nil {
			u.Prev.restore(&d)
		}
		if u.Op == "read" {
			d.Extracted = ""
		}
		if err := store.UpdateDocumentTx(tx, d); err != nil {
			return err
		}
		return store.MoveDocumentObservationsTx(tx, d.ID, d.PersonID)
	case "profile":
		p, err := store.PersonTx(tx, u.PersonID)
		if err != nil {
			return err
		}
		p.Name = u.PrevName
		if u.PrevProfile != nil {
			p.Profile = store.Profile{}
			if err := json.Unmarshal(*u.PrevProfile, &p.Profile); err != nil {
				return err
			}
		}
		return store.UpdatePersonTx(tx, p)
	}
	return fmt.Errorf("unknown change %q", u.Op)
}
