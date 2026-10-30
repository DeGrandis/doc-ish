package healthexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Invented exports, for tests and the benchmark: the shapes of the real
// ones, with readings made up so every expected value is known. The person
// is Jordan Example, born 1979-06-02, as in the benchmark's household.
//
// The Apple one covers 2026-07-01 to 2026-09-30 (UTC-7):
//   - blood pressure every morning, each listed twice (alone and as a
//     pair, as Apple does): monthly means 138/88, 134/86, 128/82
//   - resting heart rate daily: monthly means about 64, 62, 60
//   - steps from a watch and a phone counting the same walks: the watch's
//     total each day, 10,000 and 8,000 on alternate days in September
//     (a mean of 9,000), 8,000 and 6,000 before
//   - sleep: 6.5 and 7.5 hours on alternate nights in September (7.0),
//     6 and 7 before, plus an "in bed" span and awake minutes that aren't sleep
//   - weight every third day, 170.6 lb falling 0.15 lb a time
//   - blood oxygen ten times a night (so it becomes a daily average)
//   - hospital records: ferritin 18 ng/mL (low), hemoglobin 11.8 g/dL (low),
//     B12 410; a latex allergy; iron deficiency anemia (and a resolved
//     bronchitis); ferrous sulfate (and a finished azithromycin); flu and
//     Tdap vaccines
//   - two ECGs, 2026-08-14 and 2026-09-20, both sinus rhythm
//   - water intake and workouts, which aren't imported

var zone = time.FixedZone("", -7*3600)

func appleStamp(t time.Time) string { return t.In(zone).Format("2006-01-02 15:04:05 -0700") }

// InventedApple is an invented Apple Health export.zip.
func InventedApple() []byte {
	var x strings.Builder
	x.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE HealthData [
<!-- HealthKit Export Version: 13 -->
<!ELEMENT HealthData (ExportDate,Me,(Record|Correlation|Workout|ActivitySummary|ClinicalRecord)*)>
<!ATTLIST HealthData locale CDATA #REQUIRED>
]>
<HealthData locale="en_US">
 <ExportDate value="2026-10-01 09:00:00 -0700"/>
 <Me HKCharacteristicTypeIdentifierDateOfBirth="1979-06-02" HKCharacteristicTypeIdentifierBiologicalSex="HKBiologicalSexFemale" HKCharacteristicTypeIdentifierBloodType="HKBloodTypeAPositive" HKCharacteristicTypeIdentifierFitzpatrickSkinType="HKFitzpatrickSkinTypeNotSet"/>
`)
	rec := func(typ, src, unit string, start, end time.Time, val string) string {
		return fmt.Sprintf(` <Record type="%s" sourceName="%s" sourceVersion="17.0" unit="%s" creationDate="%s" startDate="%s" endDate="%s" value="%s"/>`+"\n",
			typ, src, unit, appleStamp(end), appleStamp(start), appleStamp(end), val)
	}
	watch, phone := "Jordan’s Apple Watch", "Jordan’s iPhone"
	first := time.Date(2026, 7, 1, 0, 0, 0, 0, zone)
	for i := 0; i < 92; i++ {
		day := first.AddDate(0, 0, i)
		odd := day.Day()%2 == 1
		pm := func(n float64) float64 {
			if odd {
				return n
			}
			return -n
		}
		month := day.Month()
		// Blood pressure at 7:30.
		sys, dia := map[time.Month]float64{7: 138, 8: 134, 9: 128}[month], map[time.Month]float64{7: 88, 8: 86, 9: 82}[month]
		at := day.Add(7*time.Hour + 30*time.Minute)
		s := rec("HKQuantityTypeIdentifierBloodPressureSystolic", "Omron Connect", "mmHg", at, at, fmt.Sprint(sys+pm(2)))
		d := rec("HKQuantityTypeIdentifierBloodPressureDiastolic", "Omron Connect", "mmHg", at, at, fmt.Sprint(dia+pm(2)))
		x.WriteString(s + d)
		fmt.Fprintf(&x, ` <Correlation type="HKCorrelationTypeIdentifierBloodPressure" sourceName="Omron Connect" creationDate="%s" startDate="%s" endDate="%s">`+"\n%s%s </Correlation>\n",
			appleStamp(at), appleStamp(at), appleStamp(at), d, s)
		// Resting heart rate.
		rhr := map[time.Month]float64{7: 64, 8: 62, 9: 60}[month] + pm(1)
		x.WriteString(rec("HKQuantityTypeIdentifierRestingHeartRate", watch, "count/min", day.Add(6*time.Hour), day.Add(22*time.Hour), fmt.Sprint(rhr)))
		// Heart rate, hourly through the day.
		for h := 8; h <= 21; h++ {
			t := day.Add(time.Duration(h) * time.Hour)
			x.WriteString(rec("HKQuantityTypeIdentifierHeartRate", watch, "count/min", t, t, fmt.Sprint(70+h%5)))
		}
		// Steps: the watch's total, and the phone's for the same walks.
		total := 7000.0
		if month == 9 {
			total = 9000
		}
		total += pm(1000)
		for q := 0; q < 4; q++ {
			t := day.Add(time.Duration(9+3*q) * time.Hour)
			x.WriteString(rec("HKQuantityTypeIdentifierStepCount", watch, "count", t, t.Add(40*time.Minute), fmt.Sprint(total/4)))
			x.WriteString(rec("HKQuantityTypeIdentifierStepCount", phone, "count", t, t.Add(40*time.Minute), fmt.Sprint((total-800)/4)))
		}
		// Sleep for the night ending this morning at 6:30.
		hours := 6.5
		if month != 9 {
			hours = 6
		}
		if !odd {
			hours++
		}
		wake := day.Add(6*time.Hour + 30*time.Minute)
		asleep := wake.Add(-time.Duration(hours * float64(time.Hour)))
		core, deep := asleep.Add(time.Duration(hours*0.6*float64(time.Hour))), asleep.Add(time.Duration(hours*0.8*float64(time.Hour)))
		x.WriteString(rec(sleepType, phone, "", asleep.Add(-20*time.Minute), wake.Add(15*time.Minute), "HKCategoryValueSleepAnalysisInBed"))
		x.WriteString(rec(sleepType, watch, "", asleep, core, "HKCategoryValueSleepAnalysisAsleepCore"))
		x.WriteString(rec(sleepType, watch, "", core, deep, "HKCategoryValueSleepAnalysisAsleepDeep"))
		x.WriteString(rec(sleepType, watch, "", deep, wake, "HKCategoryValueSleepAnalysisAsleepREM"))
		x.WriteString(rec(sleepType, watch, "", wake, wake.Add(10*time.Minute), "HKCategoryValueSleepAnalysisAwake"))
		// Blood oxygen through the night.
		for k := 0; k < 10; k++ {
			t := asleep.Add(time.Duration(k*30) * time.Minute)
			x.WriteString(rec("HKQuantityTypeIdentifierOxygenSaturation", watch, "%", t, t, fmt.Sprint(0.94+float64(k%5)/100)))
		}
		// Weight every third day.
		if i%3 == 0 {
			t := day.Add(7 * time.Hour)
			x.WriteString(rec("HKQuantityTypeIdentifierBodyMass", "Withings", "lb", t, t, fmt.Sprintf("%.2f", 170.6-0.15*float64(i/3))))
		}
		if i < 5 {
			t := day.Add(12 * time.Hour)
			x.WriteString(rec("HKQuantityTypeIdentifierDietaryWater", phone, "mL", t, t, "500"))
		}
		if i%30 == 0 {
			t := day.Add(18 * time.Hour)
			fmt.Fprintf(&x, ` <Workout workoutActivityType="HKWorkoutActivityTypeWalking" duration="30" durationUnit="min" sourceName="%s" startDate="%s" endDate="%s"/>`+"\n",
				watch, appleStamp(t), appleStamp(t.Add(30*time.Minute)))
		}
	}
	x.WriteString(` <ClinicalRecord type="HKClinicalTypeIdentifierLabResultRecord" identifier="1" sourceName="Example Health" fhirVersion="4.0.1" receivedDate="2025-09-13 10:00:00 -0700" resourceFilePath="/clinical-records/Observation-ferritin.json"/>` + "\n")
	x.WriteString("</HealthData>\n")

	files := map[string][]byte{"apple_health_export/export.xml": []byte(x.String())}
	subject := obj{"reference": "Patient/1", "display": "Jordan Example"}
	lab := func(name, loinc string, v float64, unit string, lo, hi float64, flag string) obj {
		r := obj{"resourceType": "Observation", "status": "final", "subject": subject,
			"category":          []any{obj{"coding": []any{obj{"code": "laboratory"}}}},
			"code":              obj{"text": name, "coding": []any{obj{"system": "http://loinc.org", "code": loinc, "display": name}}},
			"effectiveDateTime": "2025-09-12T08:30:00-07:00", "valueQuantity": obj{"value": v, "unit": unit},
			"referenceRange": []any{obj{"low": obj{"value": lo}, "high": obj{"value": hi}}}}
		if flag != "" {
			r["interpretation"] = []any{obj{"coding": []any{obj{"code": flag}}}}
		}
		return r
	}
	records := map[string]obj{
		"Observation-ferritin":   lab("Ferritin", "2276-4", 18, "ng/mL", 30, 400, "L"),
		"Observation-hemoglobin": lab("Hemoglobin", "718-7", 11.8, "g/dL", 12, 15.5, "L"),
		"Observation-b12":        lab("Vitamin B12", "2132-9", 410, "pg/mL", 200, 900, "N"),
		"Observation-bp": {"resourceType": "Observation", "status": "final", "subject": subject,
			"code": obj{"text": "Blood pressure panel"}, "effectiveDateTime": "2025-09-12T08:20:00-07:00",
			"component": []any{
				obj{"code": obj{"coding": []any{obj{"system": "http://loinc.org", "code": "8480-6", "display": "Systolic"}}}, "valueQuantity": obj{"value": 124, "unit": "mmHg"}},
				obj{"code": obj{"coding": []any{obj{"system": "http://loinc.org", "code": "8462-4", "display": "Diastolic"}}}, "valueQuantity": obj{"value": 80, "unit": "mmHg"}},
			}},
		"AllergyIntolerance-latex": {"resourceType": "AllergyIntolerance", "patient": subject,
			"clinicalStatus": obj{"coding": []any{obj{"code": "active"}}}, "code": obj{"text": "Latex"},
			"reaction": []any{obj{"manifestation": []any{obj{"text": "Contact dermatitis"}}}}},
		"Condition-anemia": {"resourceType": "Condition", "subject": subject, "clinicalStatus": obj{"coding": []any{obj{"code": "active"}}},
			"code": obj{"text": "Iron deficiency anemia"}, "onsetDateTime": "2025-09-12"},
		"Condition-bronchitis": {"resourceType": "Condition", "subject": subject, "clinicalStatus": obj{"coding": []any{obj{"code": "resolved"}}},
			"code": obj{"text": "Acute bronchitis"}, "onsetDateTime": "2024-02-03"},
		"MedicationRequest-iron": {"resourceType": "MedicationRequest", "status": "active", "subject": subject,
			"medicationCodeableConcept": obj{"text": "Ferrous sulfate 325 mg oral tablet"}, "authoredOn": "2025-09-14",
			"dosageInstruction": []any{obj{"text": "Take 1 tablet by mouth daily"}}},
		"MedicationRequest-azithro": {"resourceType": "MedicationRequest", "status": "completed", "subject": subject,
			"medicationCodeableConcept": obj{"text": "Azithromycin 250 mg oral tablet"}, "authoredOn": "2024-02-03"},
		"Immunization-flu": {"resourceType": "Immunization", "status": "completed", "patient": subject,
			"vaccineCode": obj{"text": "Influenza vaccine"}, "occurrenceDateTime": "2025-10-05"},
		"Immunization-tdap": {"resourceType": "Immunization", "status": "completed", "patient": subject,
			"vaccineCode": obj{"text": "Tdap"}, "occurrenceDateTime": "2021-04-12"},
	}
	for name, r := range records {
		b, _ := json.MarshalIndent(r, "", "  ")
		files["apple_health_export/clinical-records/"+name+".json"] = b
	}
	for _, e := range []struct{ day, at string }{{"2026-08-14", "2026-08-14 19:05:12 -0700"}, {"2026-09-20", "2026-09-20 07:42:10 -0700"}} {
		var c strings.Builder
		fmt.Fprintf(&c, "Name,Jordan Example\nDate of Birth,\"Jun 2, 1979\"\nRecorded Date,%s\nClassification,Sinus Rhythm\nSymptoms,\nSoftware Version,2\nDevice,\"Watch7,1\"\nSample Rate,512 hertz\n\nLead,Lead I\nUnit,µV\n\n", e.at)
		for k := 0; k < 200; k++ {
			fmt.Fprintf(&c, "%d.%03d\n", (k*37)%400-200, k%1000)
		}
		files["apple_health_export/electrocardiograms/ecg_"+e.day+".csv"] = []byte(c.String())
	}
	return zipOf(files)
}

// InventedGoogle is an invented Google Fit Takeout for September 2026
// (UTC-7): daily rows (steps 9,000 on average), weights in kg twice (raw
// and merged, as Takeout lists them), blood pressure, and sleep from two
// data sources that saw the same nights (7.0 hours on average).
func InventedGoogle() []byte {
	files := map[string][]byte{}
	var sum strings.Builder
	sum.WriteString("Date,Move Minutes count,Calories (kcal),Distance (m),Heart Points,Heart Minutes,Average heart rate (bpm),Max heart rate (bpm),Min heart rate (bpm),Step count,Average weight (kg),Max weight (kg),Min weight (kg)\n")
	var weights, bps, sleeps []any
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	ns := func(t time.Time) string { return fmt.Sprint(t.UnixNano()) }
	for i := 0; i < 30; i++ {
		day := first.AddDate(0, 0, i)
		odd := day.Day()%2 == 1
		steps, hours := 8000, 7.5
		if odd {
			steps, hours = 10000, 6.5
		}
		fmt.Fprintf(&sum, "%s,45,2100.5,%d,30,25,72.5,130,55,%d,,,\n", day.Format("2006-01-02"), steps*7/10, steps)
		if i%5 == 0 {
			t := day.Add(7 * time.Hour)
			weights = append(weights, obj{"dataTypeName": "com.google.weight", "startTimeNanos": ns(t), "endTimeNanos": ns(t),
				"fitValue": []any{obj{"value": obj{"fpVal": 75.0 - float64(i)/10}}}})
		}
		t := day.Add(7*time.Hour + 30*time.Minute)
		bps = append(bps, obj{"dataTypeName": "com.google.blood_pressure", "startTimeNanos": ns(t), "endTimeNanos": ns(t),
			"fitValue": []any{obj{"value": obj{"fpVal": 128.0}}, obj{"value": obj{"fpVal": 82.0}}, obj{"value": obj{}}}})
		wake := day.Add(6*time.Hour + 30*time.Minute)
		start := wake.Add(-time.Duration(hours * float64(time.Hour)))
		mid := start.Add(time.Duration(hours / 2 * float64(time.Hour)))
		sleeps = append(sleeps,
			obj{"dataTypeName": "com.google.sleep.segment", "startTimeNanos": ns(start), "endTimeNanos": ns(mid), "fitValue": []any{obj{"value": obj{"intVal": 4}}}},
			obj{"dataTypeName": "com.google.sleep.segment", "startTimeNanos": ns(mid), "endTimeNanos": ns(wake), "fitValue": []any{obj{"value": obj{"intVal": 5}}}},
			obj{"dataTypeName": "com.google.sleep.segment", "startTimeNanos": ns(wake), "endTimeNanos": ns(wake.Add(20 * time.Minute)), "fitValue": []any{obj{"value": obj{"intVal": 1}}}})
	}
	files["Takeout/Fit/Daily activity metrics/Daily activity metrics.csv"] = []byte(sum.String())
	files["Takeout/Fit/Daily activity metrics/2026-09-01.csv"] = []byte("Start time,End time,Move Minutes count,Step count\n00:00:00.000-07:00,00:15:00.000-07:00,,\n")
	points := func(src string, ps []any) []byte {
		b, _ := json.MarshalIndent(obj{"Data Source": src, "Data Points": ps}, "", "  ")
		return b
	}
	files["Takeout/Fit/All Data/derived_com.google.weight_com.google.android.gms_merge_weight.json"] = points("derived:com.google.weight:com.google.android.gms:merge_weight", weights)
	files["Takeout/Fit/All Data/raw_com.google.weight_com.example.scale.json"] = points("raw:com.google.weight:com.example.scale", weights)
	files["Takeout/Fit/All Data/raw_com.google.blood_pressure_com.example.cuff.json"] = points("raw:com.google.blood_pressure:com.example.cuff", bps)
	files["Takeout/Fit/All Data/derived_com.google.sleep.segment_com.google.android.gms_merged.json"] = points("derived:com.google.sleep.segment:merged", sleeps)
	files["Takeout/Fit/All Data/raw_com.google.sleep.segment_com.example.ring.json"] = points("raw:com.google.sleep.segment:com.example.ring", sleeps)
	files["Takeout/Fit/Activities/2026-09-05T18_00_00-07_00_Walking.tcx"] = []byte("<TrainingCenterDatabase/>")
	return zipOf(files)
}

func zipOf(files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, b := range files {
		w, _ := zw.Create(name)
		w.Write(b)
	}
	zw.Close()
	return buf.Bytes()
}
