package app

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/store"
)

// A person's history on one line: files by the date they're about, the
// dates in the profile (a medication started, a diagnosis, a surgery, a
// vaccine), and follow-ups done; with what's coming up kept apart.

type timelineEvent struct {
	Date       string   `json:"date"` // YYYY-MM-DD, or YYYY when only the year is known
	Type       string   `json:"type"` // file, medication, condition, surgery, vaccination, followup
	Kind       string   `json:"kind"` // a file's kind
	Title      string   `json:"title"`
	Detail     string   `json:"detail,omitempty"`
	DocID      int64    `json:"doc_id,omitempty"`
	Status     string   `json:"status,omitempty"` // a file waiting in Review
	Flags      []string `json:"flags,omitempty"`  // values out of range
	MoreFlags  int      `json:"more_flags,omitempty"`
	FollowUpID int64    `json:"followup_id,omitempty"`
}

const maxFlags = 4

var yearIn = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// whenOf is a profile entry's date: a whole date, or the year in it.
func whenOf(it store.Item) string {
	if d := itemDate(it.Since); d != "" {
		return d
	}
	if y := yearIn.FindString(it.Since); y != "" {
		return y
	}
	return yearIn.FindString(it.Detail)
}

func flagText(o store.Observation) string {
	v := o.ValueText
	if o.Value != nil {
		v = strconv.FormatFloat(*o.Value, 'f', -1, 64)
	}
	s := strings.TrimSpace(fmt.Sprintf("%s %s %s", o.Name, v, o.Unit))
	if f := strings.TrimSpace(o.Flag); f != "" {
		s += " (" + f + ")"
	}
	return s
}

func (a *App) handleTimeline(w http.ResponseWriter, r *http.Request) {
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
		docs, err := st.Documents(id, "", 5000)
		if err != nil {
			storeErr(w, err)
			return
		}
		flagged, err := st.FlaggedByDocument(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		follows, err := st.FollowUps(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		events := []timelineEvent{}
		for _, d := range docs {
			if d.Status != store.StatusDone && d.Status != store.StatusReview {
				continue
			}
			e := timelineEvent{Date: d.DocDate, Type: "file", Kind: d.Kind, Title: d.Title, Detail: d.Summary, DocID: d.ID}
			if e.Date == "" {
				e.Date = d.Added[:10]
			}
			if e.Title == "" {
				e.Title = d.Filename
			}
			if d.Status == store.StatusReview {
				e.Status = d.Status
			}
			for i, o := range flagged[d.ID] {
				if i == maxFlags {
					e.MoreFlags = len(flagged[d.ID]) - maxFlags
					break
				}
				e.Flags = append(e.Flags, flagText(o))
			}
			events = append(events, e)
		}
		add := func(typ, prefix string, items []store.Item) {
			for _, it := range items {
				if when := whenOf(it); when != "" {
					events = append(events, timelineEvent{Date: when, Type: typ, Title: prefix + it.Name, Detail: it.Detail})
				}
			}
		}
		pr := p.Profile
		add("medication", "Started ", pr.Medications)
		add("condition", "Diagnosed: ", pr.Conditions)
		add("surgery", "Surgery: ", pr.Surgeries)
		add("vaccination", "Vaccine: ", pr.Vaccinations)
		coming := []store.FollowUp{}
		for _, f := range follows {
			switch f.Status {
			case store.FollowUpOpen:
				coming = append(coming, f)
			case store.FollowUpDone:
				detail := f.DoneNote
				if f.When != "" {
					detail = strings.TrimSpace(fmt.Sprintf("%s. Asked for %s, on %s.", detail, f.When, f.From))
				}
				events = append(events, timelineEvent{Date: f.DoneDate, Type: "followup", Title: "Done: " + f.What, Detail: detail,
					FollowUpID: f.ID})
			}
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].Date > events[j].Date })
		writeJSON(w, map[string]any{"coming": coming, "events": events})
	})
}
