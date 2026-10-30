package ingest

import "testing"

func TestSections(t *testing.T) {
	v := func(name, date string) Value { f := 1.0; return Value{Name: name, Value: &f, Date: date} }
	lab := func(page int, date string, starts bool) PageLabel {
		return PageLabel{Page: page, NewRecord: starts, Kind: "lab", Title: "Cumulative laboratory results", Date: date}
	}
	parts := []partResult{
		{part: part{first: 1, last: 3}, res: Result{Confidence: "high",
			Pages: []PageLabel{{Page: 1, NewRecord: true, Kind: "other", Title: "Health records release"},
				{Page: 2, NewRecord: true, Kind: "visit", Title: "Visit note", Date: "2025-03-10", About: "Annual physical."},
				{Page: 3, Kind: "visit", Title: "Visit note", Date: "2025-03-10", About: "Assessment and plan."}},
			Observations: []Value{v("Blood pressure", "2025-03-10")},
			FollowUps:    []FollowUp{{What: "Follow-up visit", When: "in six months"}}, DocDate: "2025-03-10"}},
		{part: part{first: 4, last: 6}, res: Result{Confidence: "high",
			Pages:        []PageLabel{lab(4, "", true), lab(5, "2025-03-10", false), lab(6, "", false)},
			Observations: []Value{v("WBC", "2025-03-10"), v("WBC", "2025-09-12"), v("WBC", "2026-03-12")}}},
		{part: part{first: 7, last: 9}, res: Result{Confidence: "high",
			Pages: []PageLabel{lab(7, "", false),
				{Page: 8, NewRecord: true, Kind: "visit", Title: "Progress note", Date: "2025-09-12"},
				{Page: 9, NewRecord: true, Kind: "visit", Title: "Plan", Date: "2025-09-12"}}, // its own heading, the same note
			Observations: []Value{v("Ferritin", "2026-03-12"), v("Blood pressure", "2025-09-12")},
			FollowUps:    []FollowUp{{What: "Recheck iron", When: "in about six months", From: "2025-09-12"}}}},
	}
	r := merge(Request{}, parts)
	want := []Section{
		{First: 1, Last: 1, Kind: "other", Title: "Health records release"},
		{First: 2, Last: 3, Kind: "visit", Title: "Visit note", Date: "2025-03-10", Summary: "Annual physical. Assessment and plan."},
		{First: 4, Last: 7, Kind: "lab", Title: "Cumulative laboratory results", Date: "2026-03-12"},
		{First: 8, Last: 9, Kind: "visit", Title: "Progress note", Date: "2025-09-12"},
	}
	if len(r.Sections) != len(want) {
		t.Fatalf("sections: %+v", r.Sections)
	}
	for i := range want {
		if r.Sections[i] != want[i] {
			t.Errorf("section %d: %+v, want %+v", i+1, r.Sections[i], want[i])
		}
	}
	got := map[string]int{}
	for _, o := range r.Observations {
		got[o.Name+" "+o.Date] = o.Section
	}
	for k, s := range map[string]int{"Blood pressure 2025-03-10": 2, "WBC 2025-03-10": 3, "WBC 2026-03-12": 3, "Ferritin 2026-03-12": 3, "Blood pressure 2025-09-12": 4} {
		if got[k] != s {
			t.Errorf("%s in section %d, want %d", k, got[k], s)
		}
	}
	if len(r.FollowUps) != 2 || r.FollowUps[0].Section != 2 || r.FollowUps[1].Section != 4 {
		t.Errorf("follow-ups: %+v", r.FollowUps)
	}

	// A file that is one record has no sections.
	one := merge(Request{}, parts[1:2])
	if one.Sections != nil || one.Observations[0].Section != 0 {
		t.Errorf("one record: %+v", one.Sections)
	}
}
