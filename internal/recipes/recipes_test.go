package recipes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A glucose meter's export: lines about the device, then the table.
func meterExport(days int) string {
	var b strings.Builder
	b.WriteString("Glucose Meter Export\nDevice: Example GM-200\nSerial: 0000\n\nDate,Time,Glucose (mg/dL),Meal\n")
	for i := 0; i < days; i++ {
		d := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		fmt.Fprintf(&b, "%s,07:05,%d,Before breakfast\n", d.Format("01/02/2006"), 90+i%30)
		fmt.Fprintf(&b, "%s,19:40,%d,After dinner\n", d.Format("01/02/2006"), 120+i%40)
	}
	return b.String()
}

const meterRecipe = `
rows = csv_dicts(skip=4)
for r in rows:
    when = datetime(r["Date"] + " " + r["Time"])
    g = number(r["Glucose (mg/dL)"])
    if when == None or g == None:
        continue
    emit("Glucose", g, unit="mg/dL", date=when, note=r["Meal"])
`

func TestRunMeterRecipe(t *testing.T) {
	out, err := Run(context.Background(), meterRecipe, meterExport(60), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 120 {
		t.Fatalf("got %d records", len(out.Records))
	}
	r := out.Records[0]
	if r.Name != "Glucose" || *r.Value != 90 || r.Date != "2026-08-01T07:05" || r.Note != "Before breakfast" {
		t.Fatalf("first record: %+v", r)
	}
	keep, problems := Validate(out.Records, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if len(keep) != 120 || len(problems) != 0 {
		t.Fatalf("validate: %d kept, %v", len(keep), problems)
	}
	s := Summarize(keep)
	if len(s) != 1 || s[0].Count != 120 || s[0].First != "2026-08-01T07:05" {
		t.Fatalf("summary: %+v", s)
	}
}

func TestRunIsContained(t *testing.T) {
	ctx := context.Background()
	for name, script := range map[string]string{
		"endless loop": "while True:\n    pass\n",
		"load":         `load("os.star", "x")`,
		"no open":      `open("/etc/passwd")`,
		"no import":    "import os\n",
	} {
		if _, err := Run(ctx, script, "", Limits{Steps: 1_000_000, Records: 10}); err == nil {
			t.Errorf("%s: ran without error", name)
		}
	}
	// Too many values is an error, not a flood.
	if _, err := Run(ctx, "for i in range(100):\n    emit('x', i, date='2026-01-01')\n", "", Limits{Steps: 1_000_000, Records: 10}); err == nil {
		t.Error("emitted past the limit")
	}
	// A cancelled context stops a long run.
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := Run(cctx, "while True:\n    pass\n", "", DefaultLimits); err == nil || !strings.Contains(err.Error(), "time") {
		t.Errorf("timeout: %v", err)
	}
}

func TestValidateExplains(t *testing.T) {
	v := 1.0
	_, problems := Validate([]Record{
		{Name: "A", Value: &v, Date: "10/01/2026"},
		{Name: "", Value: &v, Date: "2026-10-01"},
		{Name: "B", Value: &v, Date: "2030-01-01"},
		{Name: "C", Value: &v, Date: "2026-10-01T07:05"},
	}, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if len(problems) != 3 || !strings.Contains(problems[0], "YYYY-MM-DD") || !strings.Contains(problems[2], "future") {
		t.Fatalf("problems: %v", problems)
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint(meterExport(10))
	b := Fingerprint(meterExport(40))
	if a == "" || a != b || !strings.Contains(a, "date | time | glucose (mg/dl) | meal") {
		t.Fatalf("fingerprints %q %q", a, b)
	}
	other := Fingerprint("Date/Time,SYS,DIA,Pulse\n2026-09-01 07:30,128,82,66\n2026-09-02 07:30,126,80,64\n")
	if other == a || !strings.Contains(other, "sys | dia | pulse") {
		t.Fatalf("blood pressure fingerprint %q", other)
	}
	if Fingerprint("just a note\nwith no table") != "" {
		t.Fatal("prose got a fingerprint")
	}
}

func TestParseNumber(t *testing.T) {
	for in, want := range map[string]float64{"1,234.5": 1234.5, "84,3": 84.3, "1.234,5": 1234.5, " 98 ": 98, "-3": -3, "0,25": 0.25, "1,234": 1234, "5.6": 5.6} {
		if got := parseNumber(in); got == nil || *got != want {
			t.Errorf("parseNumber(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"<0.5", "", "abc", "12 mg", ",", "1,2,3"} {
		if got := parseNumber(in); got != nil {
			t.Errorf("parseNumber(%q) = %v, want nil", in, *got)
		}
	}
}

func TestDataRows(t *testing.T) {
	if n := DataRows(meterExport(30)); n != 60 {
		t.Fatalf("meter rows: %d", n)
	}
}

func TestHelpers(t *testing.T) {
	out, err := Run(context.Background(), `
emit("a", number("1,234.5"), date=date("Mar 3, 2026"))
emit("b", number("<0.5"), text="<0.5", date=date("03/04/2026", day_first=True))
emit("c", 1, date=datetime("2026-03-05 7:15 PM"))
d = json.decode('{"x": 2}')
emit("d", d["x"], date="2026-03-06")
`, "", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Records
	if *r[0].Value != 1234.5 || r[0].Date != "2026-03-03" || r[1].Value != nil || r[1].Date != "2026-04-03" ||
		r[2].Date != "2026-03-05T19:15" || *r[3].Value != 2 {
		t.Fatalf("records: %+v", r)
	}
}
