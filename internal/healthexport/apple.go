package healthexport

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// Apple Health's export.zip holds apple_health_export/export.xml (every
// reading, one <Record> each), clinical-records/*.json (hospital records
// in FHIR, if the person connected any) and electrocardiograms/*.csv.

type appleType struct {
	name  string
	k     kind
	scale float64 // Apple stores percentages as fractions
	unit  string  // "" keeps Apple's own, tidied
}

var appleTypes = map[string]appleType{
	"HKQuantityTypeIdentifierBodyMass":                 {"Weight", point, 1, ""},
	"HKQuantityTypeIdentifierHeight":                   {"Height", point, 1, ""},
	"HKQuantityTypeIdentifierBodyMassIndex":            {"BMI", point, 1, ""},
	"HKQuantityTypeIdentifierBodyFatPercentage":        {"Body fat", point, 100, "%"},
	"HKQuantityTypeIdentifierLeanBodyMass":             {"Lean body mass", point, 1, ""},
	"HKQuantityTypeIdentifierBloodPressureSystolic":    {"Blood pressure systolic", point, 1, "mmHg"},
	"HKQuantityTypeIdentifierBloodPressureDiastolic":   {"Blood pressure diastolic", point, 1, "mmHg"},
	"HKQuantityTypeIdentifierBloodGlucose":             {"Glucose", point, 1, ""},
	"HKQuantityTypeIdentifierOxygenSaturation":         {"Blood oxygen", point, 100, "%"},
	"HKQuantityTypeIdentifierBodyTemperature":          {"Body temperature", point, 1, ""},
	"HKQuantityTypeIdentifierRestingHeartRate":         {"Resting heart rate", point, 1, "bpm"},
	"HKQuantityTypeIdentifierWalkingHeartRateAverage":  {"Walking heart rate", point, 1, "bpm"},
	"HKQuantityTypeIdentifierVO2Max":                   {"VO2 max", point, 1, "mL/kg/min"},
	"HKQuantityTypeIdentifierHeartRate":                {"Heart rate", avg, 1, "bpm"},
	"HKQuantityTypeIdentifierHeartRateVariabilitySDNN": {"Heart rate variability", avg, 1, "ms"},
	"HKQuantityTypeIdentifierRespiratoryRate":          {"Respiratory rate", avg, 1, "breaths/min"},
	"HKQuantityTypeIdentifierStepCount":                {"Steps", sum, 1, "steps"},
	"HKQuantityTypeIdentifierDistanceWalkingRunning":   {"Walking and running distance", sum, 1, ""},
	"HKQuantityTypeIdentifierActiveEnergyBurned":       {"Active energy", sum, 1, "kcal"},
	"HKQuantityTypeIdentifierFlightsClimbed":           {"Flights climbed", sum, 1, "flights"},
	"HKQuantityTypeIdentifierAppleExerciseTime":        {"Exercise minutes", sum, 1, "min"},
}

const sleepType = "HKCategoryTypeIdentifierSleepAnalysis"

func tidyUnit(u string) string {
	switch {
	case u == "degF":
		return "°F"
	case u == "degC":
		return "°C"
	case u == "count" || u == "count/min":
		return ""
	case u == "Cal":
		return "kcal"
	case strings.HasPrefix(u, "mmol<") && strings.HasSuffix(u, ">/L"):
		return "mmol/L"
	}
	return u
}

// shortType turns HKQuantityTypeIdentifierDietaryWater into DietaryWater.
func shortType(t string) string {
	for _, p := range []string{"HKQuantityTypeIdentifier", "HKCategoryTypeIdentifier", "HKDataType", "HKCorrelationTypeIdentifier"} {
		t = strings.TrimPrefix(t, p)
	}
	return t
}

// appleTime reads "2026-09-01 08:15:23 -0700", keeping the time zone it
// was recorded in.
func appleTime(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02 15:04:05 -0700", s)
	return t, err == nil
}

func parseAppleZip(ctx context.Context, zr *zip.Reader) (*Export, error) {
	a := newAcc(ctx)
	ex := &Export{Source: "Apple Health"}
	var found bool
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if strings.Contains(n, "__macosx") {
			continue
		}
		switch {
		case path.Base(n) == "export.xml":
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = parseAppleXML(a, ex, rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			found = true
		case strings.Contains(n, "clinical-records/") && strings.HasSuffix(n, ".json"):
			b, err := readEntry(f, 16<<20)
			if err == nil {
				fhirResource(a, ex, b)
			}
		case strings.Contains(n, "electrocardiograms/") && strings.HasSuffix(n, ".csv"):
			b, err := readEntry(f, 1<<20) // the header is at the top; the trace isn't needed
			if err == nil {
				appleECG(a, ex, b)
			}
		}
	}
	if !found {
		return nil, ErrNotExport
	}
	return a.finish(ex), nil
}

// ParseAppleXML reads export.xml on its own.
func ParseAppleXML(ctx context.Context, r io.Reader) (*Export, error) {
	a := newAcc(ctx)
	ex := &Export{Source: "Apple Health"}
	if err := parseAppleXML(a, ex, r); err != nil {
		return nil, err
	}
	return a.finish(ex), nil
}

func parseAppleXML(a *acc, ex *Export, r io.Reader) error {
	dec := xml.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	dec.Strict = false // some exports have stray characters in device names
	sawRoot := false
	for {
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("the export.xml in it is damaged: " + err.Error())
		}
		if err := a.tick(); err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		at := func(name string) string {
			for _, x := range se.Attr {
				if x.Name.Local == name {
					return x.Value
				}
			}
			return ""
		}
		switch se.Name.Local {
		case "HealthData":
			sawRoot = true
		case "ExportDate":
			if t, ok := appleTime(at("value")); ok {
				ex.Exported = t.Format("2006-01-02")
			}
		case "Me":
			ex.Born = at("HKCharacteristicTypeIdentifierDateOfBirth")
			switch at("HKCharacteristicTypeIdentifierBiologicalSex") {
			case "HKBiologicalSexFemale":
				ex.Sex = "Female"
			case "HKBiologicalSexMale":
				ex.Sex = "Male"
			case "HKBiologicalSexOther":
				ex.Sex = "Other"
			}
			ex.BloodType = appleBloodType(at("HKCharacteristicTypeIdentifierBloodType"))
		case "Record":
			appleRecord(a, at("type"), at("value"), at("unit"), at("startDate"), at("endDate"), at("sourceName"))
		case "Workout":
			a.skipped["workouts"]++
		}
	}
	if !sawRoot {
		return ErrNotExport
	}
	return nil
}

func appleBloodType(s string) string {
	s = strings.TrimPrefix(s, "HKBloodType")
	for _, sign := range []string{"Positive", "Negative"} {
		if g, ok := strings.CutSuffix(s, sign); ok && g != "" {
			return g + " " + strings.ToLower(sign)
		}
	}
	return ""
}

func appleRecord(a *acc, typ, val, unit, start, end, source string) {
	t, ok := appleTime(start)
	if !ok {
		return
	}
	if typ == sleepType {
		// Asleep, AsleepCore, AsleepDeep, AsleepREM: time asleep, counted
		// to the morning the night ends. In bed and awake aren't sleep.
		e, ok := appleTime(end)
		if ok && strings.Contains(val, "Asleep") {
			a.add("Sleep", "h", e.Format("2006-01-02"), source, e.Sub(t).Hours())
		}
		return
	}
	m, ok := appleTypes[typ]
	if !ok {
		a.skipped[shortType(typ)]++
		return
	}
	v, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return
	}
	v *= m.scale
	u := m.unit
	if u == "" {
		u = tidyUnit(unit)
	}
	switch m.k {
	case point:
		a.point(m.name, u, t.Format("2006-01-02T15:04"), v)
	case sum:
		a.add(m.name, u, t.Format("2006-01-02"), source, v)
	case avg:
		a.avg(m.name, u, t.Format("2006-01-02"), v)
	}
}

// appleECG reads the header of one ECG recording:
//
//	Name,Jordan Example
//	Recorded Date,2026-09-20 07:42:10 -0700
//	Classification,Sinus Rhythm
func appleECG(a *acc, ex *Export, b []byte) {
	head := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ",")
		if !ok || k == "" {
			if line == "" && len(head) > 0 {
				break
			}
			continue
		}
		if _, err := strconv.ParseFloat(k, 64); err == nil {
			break // the trace has started
		}
		head[strings.ToLower(k)] = strings.Trim(v, `" `)
	}
	t, ok := appleTime(head["recorded date"])
	if !ok {
		return
	}
	class := head["classification"]
	if class == "" {
		class = "No classification"
	}
	if n := head["name"]; n != "" {
		ex.Names = append(ex.Names, n)
	}
	r := Reading{Name: "ECG", Text: class, When: t.Format("2006-01-02T15:04"), Note: "Apple Watch ECG"}
	if hr := head["average heart rate"]; hr != "" {
		r.Note += ", average heart rate " + hr
	}
	a.done = append(a.done, r)
	ex.ECGs++
}
