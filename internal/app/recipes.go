package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DeGrandis/doc-ish/internal/blobs"
	"github.com/DeGrandis/doc-ish/internal/ingest"
	"github.com/DeGrandis/doc-ish/internal/recipes"
	"github.com/DeGrandis/doc-ish/internal/store"
)

// fullText re-reads a stored table's whole text. Callers hold a.mu.
func fullText(ctx context.Context, files *blobs.Store, d store.Document) (string, string, error) {
	rc, err := files.Open(d.Blob)
	if err != nil {
		return "", "", err
	}
	defer rc.Close()
	cont, err := ingest.Extract(ctx, d.Mime, d.Filename, rc, tmpDir())
	if err != nil {
		return "", "", err
	}
	if cont.Full == "" {
		return "", "", errors.New("recipes read tables, and this file isn't one")
	}
	return cont.Full, recipes.Fingerprint(cont.Full), nil
}

// handleApproveRecipe approves a draft: it reads its whole file now, and
// every later file in the same format without the model.
func (a *App) handleApproveRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		rc, err := st.Recipe(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if rc.Status != store.RecipeProposed || rc.DocumentID == nil {
			writeErr(w, 400, "this recipe isn't waiting for approval")
			return
		}
		d, err := st.Document(*rc.DocumentID)
		if err != nil {
			storeErr(w, err)
			return
		}
		text, _, err := fullText(r.Context(), files, d)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		recs, problems, err := runRecipe(r.Context(), rc.Script, text)
		if err != nil || len(recs) == 0 {
			if err == nil {
				err = errors.New("it found nothing")
			}
			writeErr(w, 400, "the recipe didn't work: "+err.Error())
			return
		}
		prev := snapshot(d)
		err = st.Tx(func(tx *sql.Tx) error {
			if err := store.SetRecipeStatusTx(tx, rc.ID, store.RecipeApproved); err != nil {
				return err
			}
			if _, err := fileRecordsTx(tx, &d, rc, recs, problems); err != nil {
				return err
			}
			return store.LogDocTx(tx, d.PersonID, d.ID, "you",
				fmt.Sprintf("Approved the import recipe “%s”: filed %d values from %s", rc.Name, len(recs), d.Filename),
				undoOp{Op: "recipe", DocumentID: d.ID, Prev: &prev, AllValues: true, RecipeID: rc.ID})
		})
		if err != nil {
			storeErr(w, err)
			return
		}
		// Other files in the same format waiting with drafts of their own
		// are read again, by this recipe now.
		a.requeueSameFormat(st, rc)
		d, _ = st.Document(d.ID)
		writeJSON(w, viewOf(st, d))
	})
}

func (a *App) requeueSameFormat(st *store.Store, approved store.Recipe) {
	waiting, err := st.Waiting(store.StatusReview)
	if err != nil {
		return
	}
	for _, d := range waiting {
		other, err := st.ProposedRecipe(d.ID)
		if err != nil || other.Fingerprint != approved.Fingerprint {
			continue
		}
		st.SetRecipeStatus(other.ID, store.RecipeRejected)
		st.SetStatus(d.ID, store.StatusNew, "")
	}
	a.wakeReader()
}

// handleRejectRecipe sets a draft aside. The file stays in Review.
func (a *App) handleRejectRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		rc, err := st.Recipe(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if rc.Status != store.RecipeProposed {
			writeErr(w, 400, "this recipe isn't waiting for approval")
			return
		}
		if err := st.SetRecipeStatus(id, store.RecipeRejected); err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// handleRewriteRecipe asks for a new draft for a file, optionally with
// what was wrong with the last one. It waits for the model (a minute or
// two at most).
func (a *App) handleRewriteRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req struct {
		Feedback string `json:"feedback"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var (
		d        store.Document
		text, fp string
		previous string
		set      Settings
		failed   bool
	)
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		var err error
		if d, err = st.Document(id); err != nil {
			storeErr(w, err)
			failed = true
			return
		}
		if text, fp, err = fullText(r.Context(), files, d); err != nil || fp == "" {
			if err == nil {
				err = errors.New("couldn't find this table's header row")
			}
			writeErr(w, 400, err.Error())
			failed = true
			return
		}
		if old, err := st.ProposedRecipe(id); err == nil {
			previous = old.Script
		}
		set = a.settings(st)
	})
	if failed {
		return
	}
	if msg := set.ready(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	c, err := set.chatter()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	draft, err := recipes.Write(ctx, c, recipes.WriteRequest{
		Model: set.chatModel(), NumCtx: set.NumCtx, Filename: d.Filename, Text: text, Today: time.Now(),
		Feedback: strings.TrimSpace(req.Feedback), Previous: previous,
	})
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		if cur, err := st.Document(id); err != nil || cur.Status != store.StatusReview {
			// It was filed or deleted while the model worked; keep nothing.
			writeErr(w, 409, "the file changed while the recipe was being written")
			return
		}
		a.propose(st, id, fp, draft)
		d, _ = st.Document(id)
		writeJSON(w, viewOf(st, d))
	})
}

func (a *App) handleRecipes(w http.ResponseWriter, r *http.Request) {
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		list, err := st.Recipes()
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, list)
	})
}

func (a *App) handleDeleteRecipe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		rc, err := st.Recipe(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if err := st.DeleteRecipe(id); err != nil {
			storeErr(w, err)
			return
		}
		st.Log(0, "you", fmt.Sprintf("Deleted the import recipe “%s” (values it filed stay)", rc.Name), nil)
		writeJSON(w, map[string]bool{"ok": true})
	})
}
