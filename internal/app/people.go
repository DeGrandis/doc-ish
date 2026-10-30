package app

import (
	"net/http"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/store"
)

// palette is the profile colors, in the order new profiles get them.
var palette = []string{"#ff7a59", "#5b8def", "#3fb68b", "#c77dff", "#f2b134", "#e05780"}

func (a *App) handlePeople(w http.ResponseWriter, r *http.Request) {
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		ps, err := st.People()
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, ps)
	})
}

func (a *App) handleAddPerson(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, 400, "enter a name")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		ps, _ := st.People()
		p, err := st.AddPerson(name, palette[len(ps)%len(palette)])
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, p)
	})
}

// personView is a profile with the latest weight, which lives in
// observations rather than the profile itself.
type personView struct {
	store.Person
	Weight *store.Observation `json:"weight,omitempty"`
}

func (a *App) handlePerson(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		p, err := st.Person(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		wt, _ := st.Latest(id, "Weight")
		writeJSON(w, personView{p, wt})
	})
}

func (a *App) handleUpdatePerson(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req store.Person
	if !readJSON(w, r, &req) {
		return
	}
	req.ID = id
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 400, "enter a name")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		old, err := st.Person(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if req.Color == "" {
			req.Color = old.Color
		}
		if err := st.UpdatePerson(req); err != nil {
			storeErr(w, err)
			return
		}
		p, _ := st.Person(id)
		writeJSON(w, p)
	})
}

func (a *App) handleDeletePerson(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		ps, err := st.People()
		if err != nil {
			storeErr(w, err)
			return
		}
		if len(ps) <= 1 {
			writeErr(w, 400, "this is the only profile; add another before deleting it")
			return
		}
		names, err := st.DeletePerson(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		for _, n := range names {
			files.Delete(n)
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

func (a *App) handleObservations(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		obs, err := st.Observations(id, r.URL.Query().Get("name"), 0)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, obs)
	})
}

func (a *App) handleAddObservation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var o store.Observation
	if !readJSON(w, r, &o) {
		return
	}
	o.PersonID = id
	o.Name = strings.TrimSpace(o.Name)
	if o.Name == "" || (o.Value == nil && strings.TrimSpace(o.ValueText) == "") {
		writeErr(w, 400, "enter what was measured and its value")
		return
	}
	if o.Observed == "" {
		o.Observed = time.Now().Format("2006-01-02")
	}
	if o.Source == "" {
		o.Source = "profile"
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		if _, err := st.Person(id); err != nil {
			storeErr(w, err)
			return
		}
		oid, err := st.AddObservation(o)
		if err != nil {
			storeErr(w, err)
			return
		}
		o.ID = oid
		writeJSON(w, o)
	})
}
