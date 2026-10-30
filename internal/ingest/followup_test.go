package ingest

import "testing"

func TestDueDate(t *testing.T) {
	for _, c := range []struct{ when, from, want string }{
		{"in about six months", "2025-09-12", "2026-03-12"},
		{"Follow-up CT in 12 months", "2026-09-15", "2027-09-15"},
		{"after another three months of diet changes", "2026-03-12", "2026-06-12"},
		{"in 6-12 months", "2026-01-31", "2026-07-31"},
		{"2 weeks", "2026-10-30", "2026-11-13"},
		{"in a year", "2026-10-30", "2027-10-30"},
		{"annually", "2026-10-30", "2027-10-30"},
		{"next fall", "2026-03-10", ""},
		{"in six months", "", ""},
	} {
		if got := DueDate(c.when, c.from); got != c.want {
			t.Errorf("DueDate(%q, %q) = %q, want %q", c.when, c.from, got, c.want)
		}
	}
}
