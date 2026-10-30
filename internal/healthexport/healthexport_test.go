package healthexport

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func parse(t *testing.T, b []byte) *Export {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := ParseZip(context.Background(), zr)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// series is every reading of one name in a month ("2026-09").
func series(ex *Export, name, month string) []Reading {
	var out []Reading
	for _, r := range ex.Readings {
		if r.Name == name && strings.HasPrefix(r.When, month) {
			out = append(out, r)
		}
	}
	return out
}

func mean(rs []Reading) float64 {
	s := 0.0
	for _, r := range rs {
		s += *r.Value
	}
	return s / float64(len(rs))
}

func TestApple(t *testing.T) {
	ex := parse(t, InventedApple())
	if ex.Source != "Apple Health" || ex.Born != "1979-06-02" || ex.Sex != "Female" || ex.BloodType != "A positive" || ex.Exported != "2026-10-01" {
		t.Fatalf("me: %+v", ex)
	}
	// Each pressure is listed twice in the file and kept once.
	sys, dia := series(ex, "Blood pressure systolic", "2026-09"), series(ex, "Blood pressure diastolic", "2026-09")
	if len(sys) != 30 || len(dia) != 30 || mean(sys) != 128 || mean(dia) != 82 {
		t.Fatalf("blood pressure: %d %d %g %g", len(sys), len(dia), mean(sys), mean(dia))
	}
	// The watch's steps, not the watch's plus the phone's.
	steps := series(ex, "Steps", "2026-09")
	if len(steps) != 30 || mean(steps) != 9000 || *series(ex, "Steps", "2026-09-01")[0].Value != 10000 {
		t.Fatalf("steps: %d %g", len(steps), mean(steps))
	}
	// Asleep only: not in bed, not awake.
	sleep := series(ex, "Sleep", "2026-09")
	if len(sleep) != 30 || mean(sleep) != 7 || *series(ex, "Sleep", "2026-09-01")[0].Value != 6.5 {
		t.Fatalf("sleep: %d %g", len(sleep), mean(sleep))
	}
	if hr := series(ex, "Heart rate", "2026-09"); len(hr) != 30 || !hr[0].Daily {
		t.Fatalf("heart rate should be one a day: %d", len(hr))
	}
	// Ten a night: a daily average instead.
	if ox := series(ex, "Blood oxygen", "2026-09-15"); len(ox) != 1 || !ox[0].Daily || ox[0].Unit != "%" {
		t.Fatalf("blood oxygen: %+v", ox)
	}
	if w := series(ex, "Weight", "2026-07-01"); len(w) != 1 || *w[0].Value != 170.6 || w[0].Unit != "lb" {
		t.Fatalf("weight: %+v", w)
	}
	if r := series(ex, "Resting heart rate", "2026-09-30"); len(r) != 1 || *r[0].Value != 59 || r[0].Unit != "bpm" {
		t.Fatalf("resting heart rate: %+v", r)
	}
	// Hospital records.
	fer := series(ex, "Ferritin", "2025-09-12")
	if len(fer) != 1 || *fer[0].Value != 18 || fer[0].Flag != "L" || *fer[0].RefLow != 30 || fer[0].Code != "2276-4" {
		t.Fatalf("ferritin: %+v", fer)
	}
	if b12 := series(ex, "Vitamin B12", "2025"); len(b12) != 1 || b12[0].Flag != "" {
		t.Fatalf("a normal result has no flag: %+v", b12)
	}
	if bp := series(ex, "Blood pressure systolic", "2025-09-12"); len(bp) != 1 || *bp[0].Value != 124 {
		t.Fatalf("a hospital's blood pressure files with the rest: %+v", bp)
	}
	names := func(items []Item) string {
		var s []string
		for _, i := range items {
			s = append(s, i.Name+"/"+i.Detail)
		}
		return strings.Join(s, ",")
	}
	if a := names(ex.Allergies); a != "Latex/Contact dermatitis" {
		t.Fatalf("allergies: %s", a)
	}
	if c := names(ex.Conditions); c != "Iron deficiency anemia/" {
		t.Fatalf("conditions (the resolved one left out): %s", c)
	}
	if m := names(ex.Medications); m != "Ferrous sulfate 325 mg oral tablet/Take 1 tablet by mouth daily" {
		t.Fatalf("medications (the finished one left out): %s", m)
	}
	if len(ex.Vaccinations) != 2 {
		t.Fatalf("vaccinations: %+v", ex.Vaccinations)
	}
	ecg := series(ex, "ECG", "2026-09-20")
	if ex.ECGs != 2 || len(ecg) != 1 || ecg[0].Text != "Sinus Rhythm" || ecg[0].When != "2026-09-20T07:42" {
		t.Fatalf("ECGs: %d %+v", ex.ECGs, ecg)
	}
	if ex.Skipped["DietaryWater"] != 5 || ex.Skipped["workouts"] != 4 {
		t.Fatalf("skipped: %v", ex.Skipped)
	}
	for _, n := range ex.Names {
		if n != "Jordan Example" {
			t.Fatalf("names: %v", ex.Names)
		}
	}
	if s := Summary(ex); !strings.Contains(s, "92 days of steps") || !strings.Contains(s, "92 blood pressure systolic readings") ||
		!strings.Contains(s, "5 values from hospital records. 2 ECGs.") || strings.Contains(s, "ferritin") {
		t.Fatalf("summary: %s", s)
	}
}

func TestAppleXMLAlone(t *testing.T) {
	zr, _ := zip.NewReader(bytes.NewReader(InventedApple()), int64(len(InventedApple())))
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "export.xml") {
			b, _ := readEntry(f, 1<<30)
			if !IsAppleXML(b[:512]) {
				t.Fatal("not recognized")
			}
			ex, err := ParseAppleXML(context.Background(), bytes.NewReader(b))
			if err != nil || len(series(ex, "Steps", "2026-09")) != 30 || ex.ECGs != 0 {
				t.Fatalf("%v", err)
			}
		}
	}
}

func TestGoogle(t *testing.T) {
	ex := parse(t, InventedGoogle())
	if ex.Source != "Google Fit" {
		t.Fatal(ex.Source)
	}
	if steps := series(ex, "Steps", "2026-09"); len(steps) != 30 || mean(steps) != 9000 {
		t.Fatalf("steps: %d %g", len(steps), mean(steps))
	}
	if w := series(ex, "Weight", "2026-09"); len(w) != 6 || *w[0].Value != 75 || w[0].Unit != "kg" || w[0].When != "2026-09-01T07:00" {
		t.Fatalf("weight (listed twice, kept once, in local time): %+v", w)
	}
	if bp := series(ex, "Blood pressure diastolic", "2026-09"); len(bp) != 30 || mean(bp) != 82 {
		t.Fatalf("blood pressure: %d", len(bp))
	}
	// Two sources saw the same nights: counted once.
	if s := series(ex, "Sleep", "2026-09"); len(s) != 30 || mean(s) != 7 {
		t.Fatalf("sleep: %d %g", len(s), mean(s))
	}
	if hr := series(ex, "Heart rate", "2026-09-02"); len(hr) != 1 || *hr[0].Value != 72.5 {
		t.Fatalf("heart rate: %+v", hr)
	}
	if d := series(ex, "Distance", "2026-09-01"); len(d) != 1 || *d[0].Value != 7 {
		t.Fatalf("distance in km: %+v", d)
	}
	if ex.Skipped["workouts"] != 1 {
		t.Fatalf("skipped: %v", ex.Skipped)
	}
}
