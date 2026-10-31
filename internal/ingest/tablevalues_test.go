package ingest

import "testing"

func TestTableValuesDates(t *testing.T) {
	text := `Some running text with 72 in it.
| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units | Range |
| Hemoglobin A1c | 5.6 | 5.7 H | 5.8 H | % | 4.0-5.6 |
| Iron |  | 38 L | 72 | ug/dL | 50-170 |
| Ferritin |  | 18 L | 41 | ng/mL | 30-400 |
| Sodium | 140 | 140 | 138 | mmol/L | 135-145 |
`
	num := func(f float64) *float64 { return &f }
	for _, tc := range []struct {
		name  string
		value float64
		date  string
		want  string
	}{
		{"Iron", 72, "2025-03-12", "2026-03-12"},     // a year the table doesn't have
		{"Ferritin", 18, "2025-03-10", "2025-09-12"}, // slid left into a blank cell
		{"Ferritin", 41, "2026-03-12", "2026-03-12"}, // right already
		{"A1c", 5.8, "2025-09-12", "2026-03-12"},     // the name in part
		{"Sodium", 140, "2025-09-12", "2025-09-12"},  // in two columns, and one of them
		{"Sodium", 140, "2026-03-12", "2026-03-12"},  // in two columns, neither: can't tell
		{"Iron", 99, "2025-03-12", "2025-03-12"},     // not in the table
		{"Weight", 72, "2025-03-12", "2025-03-12"},   // no such row
		{"Glucose", 5.6, "2025-03-12", "2025-03-12"}, // another row's value
		{"Iron", 72, "", "2026-03-12"},               // no date given
	} {
		vals := []Value{{Name: tc.name, Value: num(tc.value), Date: tc.date}}
		vals = tableValues(text, vals)
		if vals[0].Date != tc.want {
			t.Errorf("%s %g on %q: got %s, want %s", tc.name, tc.value, tc.date, vals[0].Date, tc.want)
		}
	}
}

// A table with one date column names nothing to choose between.
func TestTableValuesLeavesOtherTables(t *testing.T) {
	v := 7.0
	vals := []Value{{Name: "Glucose", Value: &v, Date: "2025-01-01"}}
	vals = tableValues("| Test | 03/10/2025 | Units |\n| Glucose | 7 | mmol/L |\n", vals)
	if len(vals) != 1 || vals[0].Date != "2025-01-01" {
		t.Fatalf("%+v", vals)
	}
}

// A cell the model skipped in a row it read comes from the table, under
// its own date, and a row it didn't read stays out.
func TestTableValuesFillsARow(t *testing.T) {
	text := `| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units | Range |
| Iron |  | 38 L | 72 | ug/dL | 50-170 |
| Page total | 1 | 2 | 3 |  |  |
`
	v, low, high := 38.0, 50.0, 170.0
	vals := tableValues(text, []Value{{Name: "Iron", Value: &v, Unit: "ug/dL", RefLow: &low, RefHigh: &high, Flag: "L", Date: "2025-09-12"}})
	if len(vals) != 2 {
		t.Fatalf("%+v", vals)
	}
	got := vals[1]
	if got.Name != "Iron" || *got.Value != 72 || got.Date != "2026-03-12" || got.Unit != "ug/dL" || got.Flag != "" || *got.RefHigh != 170 {
		t.Fatalf("%+v", got)
	}
	// The blank first cell stays blank, and a second pass adds nothing.
	if again := tableValues(text, vals); len(again) != 2 {
		t.Fatalf("%+v", again)
	}
}
