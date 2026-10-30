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
	Type       string   `json:"type"` // file, section (a record inside a long file), medication, condition, surgery, vaccination, followup
	Kind       string   `json:"kind"` // a file's kind
	Title      string   `json:"title"`
	Detail     string   `json:"detail,omitempty"`
	DocID      int64    `json:"doc_id,omitempty"`
	Page       int      `json:"page,omitempty"`   // where a section starts in its file
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

// flagsOf is the first few out-of-range values, and how many more.
func flagsOf(obs []store.Observation) ([]string, int) {
	var out []string
	for i, o := range obs {
		if i == maxFlags {
			return out, len(obs) - maxFlags
		}
		out = append(out, flagText(o))
	}
	return out, 0
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
		all, err := st.PersonSections(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		days, err := st.SectionDays(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		secs := map[int64][]store.Section{}
		for _, s := range all {
			secs[s.DocumentID] = append(secs[s.DocumentID], s)
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
			if len(secs[d.ID]) == 0 {
				e.Flags, e.MoreFlags = flagsOf(flagged[d.ID])
				events = append(events, e)
				continue
			}
			// A file of many records: the file on its newest date, saying
			// what's in it, and each dated record on its own date, a lab
			// table once for each day it has results for.
			e.Detail = strings.TrimSpace(fmt.Sprintf("%d records inside, by page. %s", len(secs[d.ID]), e.Detail))
			events = append(events, e)
			bySection := map[int64][]store.Observation{}
			for _, o := range flagged[d.ID] {
				if o.SectionID != nil {
					bySection[*o.SectionID] = append(bySection[*o.SectionID], o)
				}
			}
			for _, s := range secs[d.ID] {
				se := timelineEvent{Type: "section", Kind: s.Kind, Title: s.Title, Detail: s.Summary, DocID: d.ID, Page: s.FirstPage, Status: e.Status}
				if se.Title == "" {
					se.Title = s.Kind
				}
				if s.Kind == "lab" && len(days[s.ID]) > 0 {
					for day, n := range days[s.ID] {
						le := se
						le.Date, le.Detail = day, fmt.Sprintf("%d result%s from this day.", n, plural(n))
						var mine []store.Observation
						for _, o := range bySection[s.ID] {
							if strings.HasPrefix(o.Observed, day) {
								mine = append(mine, o)
							}
						}
						le.Flags, le.MoreFlags = flagsOf(mine)
						events = append(events, le)
					}
					continue
				}
				if s.Date == "" {
					continue // undated (a handout, a form): listed in the file
				}
				se.Date = s.Date
				se.Flags, se.MoreFlags = flagsOf(bySection[s.ID])
				events = append(events, se)
			}
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
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].Date != events[j].Date {
				return events[i].Date > events[j].Date
			}
			return events[i].Page < events[j].Page
		})
		writeJSON(w, map[string]any{"coming": coming, "events": events})
	})
}
