package healthexport

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Google Fit's Takeout holds, under Takeout/Fit/:
//
//	Daily activity metrics/Daily activity metrics.csv   one row a day: steps, distance, calories,
//	                                                    move minutes, heart points, heart rate, weight
//	Daily activity metrics/2026-09-01.csv               the same day in 15-minute rows
//	All Data/derived_com.google.weight_….json           every data point of one type:
//	    {"Data Source": "…", "Data Points": [{"dataTypeName": "com.google.weight",
//	      "startTimeNanos": …, "endTimeNanos": …, "fitValue": [{"value": {"fpVal": 72.5}}]}]}
//	Activities/*.tcx                                    workouts
//
// Days come from the daily rows; one-by-one readings (weight, blood
// pressure, glucose…) and sleep from the data points.

type fitType struct {
	name  string
	scale float64
	unit  string
}

var fitTypes = map[string]fitType{
	"com.google.weight":              {"Weight", 1, "kg"},
	"com.google.height":              {"Height", 100, "cm"},
	"com.google.body.fat.percentage": {"Body fat", 1, "%"},
	"com.google.blood_glucose":       {"Glucose", 1, "mmol/L"},
	"com.google.oxygen_saturation":   {"Blood oxygen", 1, "%"},
	"com.google.body.temperature":    {"Body temperature", 1, "°C"},
}

type fitPoint struct {
	Type     string          `json:"dataTypeName"`
	Start    json.RawMessage `json:"startTimeNanos"`
	End      json.RawMessage `json:"endTimeNanos"`
	FitValue []struct {
		Value struct {
			FpVal  *float64 `json:"fpVal"`
			IntVal *int64   `json:"intVal"`
		} `json:"value"`
	} `json:"fitValue"`
}

func nanos(raw json.RawMessage, loc *time.Location) (time.Time, bool) {
	n, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n).In(loc), true
}

var dayFile = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.csv$`)

func parseGoogleZip(ctx context.Context, zr *zip.Reader) (*Export, error) {
	a := newAcc(ctx)
	ex := &Export{Source: "Google Fit"}
	var summary *zip.File
	var days, points []*zip.File
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		base := path.Base(f.Name)
		switch {
		case !strings.Contains(n, "fit/"):
		case strings.Contains(n, "daily activity metrics") && strings.EqualFold(base, "Daily activity metrics.csv"):
			summary = f
		case strings.Contains(n, "daily activity metrics") && dayFile.MatchString(base):
			days = append(days, f)
		case strings.Contains(n, "/all data/") && strings.HasSuffix(n, ".json"):
			points = append(points, f)
		case strings.HasSuffix(n, ".tcx"):
			a.skipped["workouts"]++
		}
	}
	loc := googleZone(days)
	weights := false
	for _, f := range points {
		got, err := fitPoints(a, f, loc)
		if err != nil {
			return nil, err
		}
		weights = weights || got
	}
	if summary != nil {
		if err := fitDays(a, summary, "", !weights); err != nil {
			return nil, err
		}
	} else {
		for _, f := range days {
			if err := fitDays(a, f, dayFile.FindStringSubmatch(path.Base(f.Name))[1], !weights); err != nil {
				return nil, err
			}
		}
	}
	return a.finish(ex), nil
}

// googleZone finds the time zone the export was made in, from a daily
// file's first start time ("00:00:00.000-07:00"); the data points are in
// UTC.
func googleZone(days []*zip.File) *time.Location {
	for _, f := range days {
		b, err := readEntry(f, 4096)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) < 2 {
			continue
		}
		first, _, _ := strings.Cut(lines[1], ",")
		if t, err := time.Parse("15:04:05.000Z07:00", strings.TrimSpace(first)); err == nil {
			_, off := t.Zone()
			return time.FixedZone("", off)
		}
	}
	return time.UTC
}

// fitPoints streams one data-point file, which can be hundreds of
// megabytes. It reports whether it found weights.
func fitPoints(a *acc, f *zip.File, loc *time.Location) (bool, error) {
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(rc, 1<<20))
	source := strings.TrimSuffix(path.Base(f.Name), ".json")
	// Find "Data Points": [
	for {
		tok, err := dec.Token()
		if err != nil {
			return false, nil // not the shape expected; leave it
		}
		if s, ok := tok.(string); ok && s == "Data Points" {
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				return false, nil
			}
			break
		}
	}
	weights := false
	for dec.More() {
		var p fitPoint
		if err := dec.Decode(&p); err != nil {
			return weights, nil
		}
		if err := a.tick(); err != nil {
			return weights, err
		}
		start, ok := nanos(p.Start, loc)
		if !ok || len(p.FitValue) == 0 {
			continue
		}
		v0 := p.FitValue[0].Value
		when := start.Format("2006-01-02T15:04")
		switch {
		case p.Type == "com.google.blood_pressure":
			if len(p.FitValue) >= 2 && v0.FpVal != nil && p.FitValue[1].Value.FpVal != nil {
				a.point("Blood pressure systolic", "mmHg", when, *v0.FpVal)
				a.point("Blood pressure diastolic", "mmHg", when, *p.FitValue[1].Value.FpVal)
			}
		case p.Type == "com.google.sleep.segment":
			// 2 sleeping, 4 light, 5 deep, 6 REM; 1 awake, 3 out of bed
			end, ok := nanos(p.End, loc)
			if ok && v0.IntVal != nil {
				switch *v0.IntVal {
				case 2, 4, 5, 6:
					a.add("Sleep", "h", end.Format("2006-01-02"), source, end.Sub(start).Hours())
				}
			}
		default:
			m, ok := fitTypes[p.Type]
			if !ok {
				a.skipped[strings.TrimPrefix(p.Type, "com.google.")]++
				continue
			}
			if v0.FpVal == nil {
				continue
			}
			a.point(m.name, m.unit, when, *v0.FpVal*m.scale)
			weights = weights || m.name == "Weight"
		}
	}
	return weights, nil
}

// fitDays reads daily rows: the summary file (a Date column) or one day's
// 15-minute rows (day given), totalled. Weights come from here only when the data points had none.
func fitDays(a *acc, f *zip.File, day string, csvWeight bool) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	r := csv.NewReader(bufio.NewReader(rc))
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil
	}
	col := func(prefix string) int {
		for i, h := range head {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, string(rune(0xfeff))))), prefix) {
				return i
			}
		}
		return -1
	}
	date, steps, dist, cal, move, points := col("date"), col("step count"), col("distance"), col("calories"), col("move minutes"), col("heart points")
	hrAvg, hrMax, hrMin, wt := col("average heart rate"), col("max heart rate"), col("min heart rate"), col("average weight")
	hr := map[string]*stat{}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if err := a.tick(); err != nil {
			return err
		}
		val := func(i int) (float64, bool) {
			if i < 0 || i >= len(row) || strings.TrimSpace(row[i]) == "" {
				return 0, false
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(row[i]), 64)
			return v, err == nil
		}
		d := day
		if d == "" && date >= 0 && date < len(row) {
			d = strings.TrimSpace(row[date])
		}
		if len(d) < 10 {
			continue
		}
		d = d[:10]
		if v, ok := val(steps); ok {
			a.add("Steps", "steps", d, "Google Fit", v)
		}
		if v, ok := val(dist); ok {
			a.add("Distance", "km", d, "Google Fit", v/1000)
		}
		if v, ok := val(cal); ok {
			a.add("Calories burned", "kcal", d, "Google Fit", v)
		}
		if v, ok := val(move); ok {
			a.add("Move minutes", "min", d, "Google Fit", v)
		}
		if v, ok := val(points); ok {
			a.add("Heart points", "", d, "Google Fit", v)
		}
		if v, ok := val(hrAvg); ok {
			s := hr[d]
			if s == nil {
				s = &stat{lo: v, hi: v}
				hr[d] = s
			}
			s.sum += v
			s.n++
			if lo, ok := val(hrMin); ok {
				s.lo = min(s.lo, lo)
			}
			if hi, ok := val(hrMax); ok {
				s.hi = max(s.hi, hi)
			}
		}
		if v, ok := val(wt); ok && csvWeight {
			a.point("Weight", "kg", d, v)
		}
	}
	for d, s := range hr {
		a.done = append(a.done, Reading{Name: "Heart rate", Value: num(round(s.sum/float64(s.n), 1)), Unit: "bpm", When: d, Daily: true,
			Note: "daily average, lowest " + strconv.FormatFloat(round(s.lo, 1), 'f', -1, 64) + ", highest " + strconv.FormatFloat(round(s.hi, 1), 'f', -1, 64)})
	}
	return nil
}
