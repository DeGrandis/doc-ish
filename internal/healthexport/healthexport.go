// Package healthexport reads the exports phones and watches make of a
// person's health data (Apple Health's export.zip, Google Fit's Takeout)
// with fixed code, no model: the formats are known, and they can hold
// millions of readings over years. Everything is streamed, so a
// several-gigabyte export is never held in memory.
//
// What comes out is shaped for the app: readings that matter one by one
// (weight, blood pressure, glucose…) are kept one by one; ones a watch
// records every few minutes (heart rate, steps, sleep) become one value a
// day; hospital records become lab results and profile entries.
package healthexport

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"
)

// Reading is one value to store.
type Reading struct {
	Name      string
	Code      string // LOINC, when a hospital record gives one
	Value     *float64
	Text      string // a value that isn't a number ("Sinus rhythm")
	Unit      string
	RefLow    *float64
	RefHigh   *float64
	Flag      string
	When      string // YYYY-MM-DD, or YYYY-MM-DDTHH:MM in the time zone it was recorded in
	Daily     bool   // one value standing for a whole day
	Note      string
	FromChart bool // from a hospital record rather than the device
}

// Item is a profile entry from hospital records.
type Item struct {
	Name, Detail, Since string
}

// Export is what was read.
type Export struct {
	Source       string // "Apple Health" or "Google Fit"
	Exported     string // the day the export was made, when it says
	Born         string // YYYY-MM-DD
	Sex          string
	BloodType    string // "A positive"
	Readings     []Reading
	Allergies    []Item
	Conditions   []Item
	Medications  []Item
	Vaccinations []Item
	Names        []string       // patient names found in hospital records and ECGs
	Clinical     int            // values from hospital records
	ECGs         int            // ECG recordings
	Skipped      map[string]int // kinds of data not imported, with how many records
}

// ErrNotExport means the file isn't an export this package knows.
var ErrNotExport = errors.New("not a health export this app knows")

// Kind says what export a zip holds: "apple", "google", or "".
func Kind(zr *zip.Reader) string {
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if strings.Contains(n, "__macosx") {
			continue
		}
		if path.Base(n) == "export.xml" {
			return "apple"
		}
		if strings.Contains(n, "fit/") && (strings.Contains(n, "/all data/") || strings.Contains(n, "daily activity metrics")) {
			return "google"
		}
	}
	return ""
}

// IsAppleXML reports whether the start of a file is Apple Health's
// export.xml.
func IsAppleXML(head []byte) bool {
	return bytes.Contains(head, []byte("<!DOCTYPE HealthData")) || bytes.Contains(head, []byte("<HealthData"))
}

// ParseZip reads an export zip of either kind.
func ParseZip(ctx context.Context, zr *zip.Reader) (*Export, error) {
	switch Kind(zr) {
	case "apple":
		return parseAppleZip(ctx, zr)
	case "google":
		return parseGoogleZip(ctx, zr)
	}
	return nil, ErrNotExport
}

// ---------- gathering ----------

type kind int

const (
	point kind = iota // each reading kept (unless there are many a day)
	sum               // one total a day
	avg               // one average a day
)

type pt struct {
	when string
	v    float64
}

type stat struct {
	sum, lo, hi float64
	n           int
}

// acc gathers readings as they stream past.
type acc struct {
	ctx     context.Context
	points  map[string][]pt
	seen    map[string]bool
	sums    map[string]map[string]map[string]float64 // name -> day -> source -> total
	avgs    map[string]map[string]*stat              // name -> day
	units   map[string]string
	order   []string
	done    []Reading // already final: hospital records, ECGs, Google's daily rows
	skipped map[string]int
	n       int
}

func newAcc(ctx context.Context) *acc {
	return &acc{ctx: ctx, points: map[string][]pt{}, seen: map[string]bool{}, sums: map[string]map[string]map[string]float64{},
		avgs: map[string]map[string]*stat{}, units: map[string]string{}, skipped: map[string]int{}}
}

// tick checks for cancellation every so often.
func (a *acc) tick() error {
	a.n++
	if a.n%20000 == 0 {
		return a.ctx.Err()
	}
	return nil
}

func (a *acc) name(n, unit string) {
	if _, ok := a.units[n]; !ok {
		a.order = append(a.order, n)
		a.units[n] = unit
	}
}

// point keeps one reading; the same reading twice (Apple lists blood
// pressure both alone and as a pair) is kept once.
func (a *acc) point(n, unit, when string, v float64) {
	k := fmt.Sprintf("%s|%s|%g", n, when, v)
	if a.seen[k] {
		return
	}
	a.seen[k] = true
	a.name(n, unit)
	a.points[n] = append(a.points[n], pt{when, v})
}

// add counts toward a day's total from one source. Phones and watches
// both count steps; the day's value is the largest source's total, not
// their sum, which would count the same steps twice.
func (a *acc) add(n, unit, day, source string, v float64) {
	a.name(n, unit)
	if a.sums[n] == nil {
		a.sums[n] = map[string]map[string]float64{}
	}
	if a.sums[n][day] == nil {
		a.sums[n][day] = map[string]float64{}
	}
	a.sums[n][day][source] += v
}

func (a *acc) avg(n, unit, day string, v float64) {
	a.name(n, unit)
	if a.avgs[n] == nil {
		a.avgs[n] = map[string]*stat{}
	}
	s := a.avgs[n][day]
	if s == nil {
		s = &stat{lo: v, hi: v}
		a.avgs[n][day] = s
	}
	s.sum += v
	s.n++
	s.lo, s.hi = math.Min(s.lo, v), math.Max(s.hi, v)
}

// manyADay is where one-by-one readings become a daily average: a glucose
// sensor every five minutes, or blood oxygen measured all night.
const manyADay = 6

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func num(v float64) *float64 { return &v }

func places(unit string) int {
	switch unit {
	case "", "steps", "kcal", "min", "flights":
		return 0
	}
	return 2
}

// readings is everything gathered, by measurement and then date.
func (a *acc) readings() []Reading {
	var out []Reading
	for _, n := range a.order {
		unit := a.units[n]
		var rs []Reading
		if ps := a.points[n]; len(ps) > 0 {
			days := map[string]bool{}
			for _, p := range ps {
				days[p.when[:10]] = true
			}
			if len(ps) > manyADay*len(days) {
				for _, p := range ps {
					a.avg(n, unit, p.when[:10], p.v)
				}
			} else {
				for _, p := range ps {
					rs = append(rs, Reading{Name: n, Value: num(round(p.v, 2)), Unit: unit, When: p.when})
				}
			}
		}
		for day, by := range a.sums[n] {
			best, from := 0.0, ""
			for src, v := range by {
				if v > best || from == "" {
					best, from = v, src
				}
			}
			r := Reading{Name: n, Value: num(round(best, places(unit))), Unit: unit, When: day, Daily: true, Note: "the day's total"}
			if len(by) > 1 {
				r.Note = fmt.Sprintf("the day's total, the largest of %d sources", len(by))
			}
			rs = append(rs, r)
		}
		for day, s := range a.avgs[n] {
			rs = append(rs, Reading{Name: n, Value: num(round(s.sum/float64(s.n), 1)), Unit: unit, When: day, Daily: true,
				Note: fmt.Sprintf("daily average of %d readings, lowest %g, highest %g", s.n, round(s.lo, 2), round(s.hi, 2))})
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].When < rs[j].When })
		out = append(out, rs...)
	}
	return append(out, a.done...)
}

func (a *acc) finish(ex *Export) *Export {
	ex.Readings = a.readings()
	ex.Skipped = a.skipped
	return ex
}

// readEntry opens one file in a zip, capped at limit bytes.
func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// Summary describes an export in a few lines, for the file's summary and
// for the assistant.
func Summary(ex *Export) string {
	type agg struct {
		n           int
		first, last string
		daily       bool
	}
	by := map[string]*agg{}
	var order []string
	for _, r := range ex.Readings {
		if r.FromChart || r.Name == "ECG" {
			continue // counted below
		}
		g := by[r.Name]
		if g == nil {
			g = &agg{first: r.When, daily: r.Daily}
			by[r.Name] = g
			order = append(order, r.Name)
		}
		g.n++
		if r.When < g.first {
			g.first = r.When
		}
		if r.When > g.last {
			g.last = r.When
		}
	}
	var parts []string
	for _, n := range order {
		g := by[n]
		what := fmt.Sprintf("%d %s reading%s", g.n, strings.ToLower(n), plural(g.n))
		if g.daily {
			what = fmt.Sprintf("%d days of %s", g.n, strings.ToLower(n))
		}
		parts = append(parts, fmt.Sprintf("%s (%s to %s)", what, g.first[:10], g.last[:10]))
	}
	s := ex.Source + " export"
	if ex.Exported != "" {
		s += " made " + ex.Exported
	}
	s += ": " + strings.Join(parts, "; ") + "."
	if ex.Clinical > 0 {
		s += fmt.Sprintf(" %d value%s from hospital records.", ex.Clinical, plural(ex.Clinical))
	}
	if ex.ECGs > 0 {
		s += fmt.Sprintf(" %d ECG%s.", ex.ECGs, plural(ex.ECGs))
	}
	return s
}

// Overview is the export as text for search and the assistant: each
// measurement month by month, then the hospital records and ECGs.
func Overview(ex *Export) string {
	var b strings.Builder
	b.WriteString(Summary(ex) + "\n")
	type month struct {
		sum, lo, hi float64
		n           int
	}
	by := map[string]map[string]*month{}
	units := map[string]string{}
	var order []string
	var chart, ecgs []Reading
	for _, r := range ex.Readings {
		switch {
		case r.FromChart:
			chart = append(chart, r)
			continue
		case r.Name == "ECG":
			ecgs = append(ecgs, r)
			continue
		case r.Value == nil:
			continue
		}
		if by[r.Name] == nil {
			by[r.Name] = map[string]*month{}
			units[r.Name] = r.Unit
			order = append(order, r.Name)
		}
		m := by[r.Name][r.When[:7]]
		if m == nil {
			m = &month{lo: *r.Value, hi: *r.Value}
			by[r.Name][r.When[:7]] = m
		}
		m.sum += *r.Value
		m.n++
		m.lo, m.hi = math.Min(m.lo, *r.Value), math.Max(m.hi, *r.Value)
	}
	for _, n := range order {
		fmt.Fprintf(&b, "\n%s (%s), month by month:\n", n, units[n])
		var ms []string
		for k := range by[n] {
			ms = append(ms, k)
		}
		sort.Strings(ms)
		for _, k := range ms {
			m := by[n][k]
			fmt.Fprintf(&b, "- %s: average %g, lowest %g, highest %g (%d)\n", k, round(m.sum/float64(m.n), 1), m.lo, m.hi, m.n)
		}
	}
	if len(chart) > 0 {
		b.WriteString("\nFrom hospital records:\n")
		for _, r := range chart {
			v := r.Text
			if r.Value != nil {
				v = fmt.Sprintf("%g %s", *r.Value, r.Unit)
			}
			fmt.Fprintf(&b, "- %s %s: %s", r.When, r.Name, v)
			if r.RefLow != nil || r.RefHigh != nil {
				fmt.Fprintf(&b, " (range %s–%s)", fnum(r.RefLow), fnum(r.RefHigh))
			}
			if r.Flag != "" {
				b.WriteString(" flag " + r.Flag)
			}
			b.WriteString("\n")
		}
	}
	for _, r := range ecgs {
		fmt.Fprintf(&b, "- ECG %s: %s\n", r.When, r.Text)
	}
	items := func(what string, list []Item) {
		for _, it := range list {
			fmt.Fprintf(&b, "- %s: %s", what, it.Name)
			if it.Detail != "" {
				b.WriteString(" (" + it.Detail + ")")
			}
			if it.Since != "" {
				b.WriteString(", " + it.Since)
			}
			b.WriteString("\n")
		}
	}
	items("Allergy", ex.Allergies)
	items("Condition", ex.Conditions)
	items("Medication", ex.Medications)
	items("Vaccination", ex.Vaccinations)
	return b.String()
}

func fnum(f *float64) string {
	if f == nil {
		return ""
	}
	return fmt.Sprint(*f)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
