package healthexport

import (
	"encoding/json"
	"strings"
	"time"
)

// Hospital records come as FHIR resources, one per file: lab results and
// vital signs (Observation), allergies, conditions, medications and
// vaccinations. Both versions phones receive (DSTU2 and R4) are read; they
// differ mostly in field names.

type obj = map[string]any

// get walks a path of keys; at a list it takes the first element.
func get(v any, keys ...string) any {
	for _, k := range keys {
		if l, ok := v.([]any); ok {
			if len(l) == 0 {
				return nil
			}
			v = l[0]
		}
		m, ok := v.(obj)
		if !ok {
			return nil
		}
		v = m[k]
	}
	if l, ok := v.([]any); ok && len(l) > 0 {
		return l[0]
	}
	return v
}

func str(v any, keys ...string) string {
	s, _ := get(v, keys...).(string)
	return strings.TrimSpace(s)
}

func float(v any, keys ...string) *float64 {
	switch x := get(v, keys...).(type) {
	case float64:
		return &x
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

// concept is a CodeableConcept's words: its text, or its first coding's
// display.
func concept(v any) string {
	if s := str(v, "text"); s != "" {
		return s
	}
	return str(v, "coding", "display")
}

// status reads a status that's a string (DSTU2) or a concept (R4).
func status(v any) string {
	if s, ok := v.(string); ok {
		return strings.ToLower(s)
	}
	return strings.ToLower(str(v, "coding", "code"))
}

// fhirDate turns "2025-09-12T08:30:00-07:00" into 2025-09-12T08:30 and
// "2025-09-12" into itself.
func fhirDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if t.Hour() == 0 && t.Minute() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format("2006-01-02T15:04")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// loincNames are vital signs worth filing under the names the app already
// uses, so a hospital's weight and the scale's chart together.
var loincNames = map[string]string{
	"29463-7": "Weight", "3141-9": "Weight", "8302-2": "Height", "39156-5": "BMI",
	"8480-6": "Blood pressure systolic", "8462-4": "Blood pressure diastolic",
	"8867-4": "Heart rate", "2708-6": "Blood oxygen", "59408-5": "Blood oxygen", "8310-5": "Body temperature",
}

func loinc(code any) string {
	m, _ := code.(obj)
	all, _ := m["coding"].([]any)
	for _, c := range all {
		if cm, ok := c.(obj); ok && strings.Contains(str(cm, "system"), "loinc") {
			return str(cm, "code")
		}
	}
	return ""
}

func fhirResource(a *acc, ex *Export, b []byte) {
	var r obj
	if json.Unmarshal(b, &r) != nil {
		return
	}
	if name := str(r, "subject", "display"); name != "" {
		ex.Names = append(ex.Names, name)
	} else if name := str(r, "patient", "display"); name != "" {
		ex.Names = append(ex.Names, name)
	}
	switch str(r, "resourceType") {
	case "Observation":
		fhirObservation(a, ex, r)
	case "AllergyIntolerance":
		if gone(status(r["clinicalStatus"]), status(r["verificationStatus"])) {
			return
		}
		name := concept(r["code"])
		if name == "" {
			name = concept(r["substance"])
		}
		if name != "" {
			ex.Allergies = append(ex.Allergies, Item{Name: name, Detail: concept(get(r, "reaction", "manifestation"))})
		}
	case "Condition":
		if gone(status(r["clinicalStatus"]), status(r["verificationStatus"])) {
			return
		}
		if name := concept(r["code"]); name != "" {
			ex.Conditions = append(ex.Conditions, Item{Name: name, Since: fhirDate(str(r, "onsetDateTime"))})
		}
	case "MedicationRequest", "MedicationStatement", "MedicationOrder":
		if s := status(r["status"]); s != "" && s != "active" && s != "on-hold" && s != "intended" {
			return // completed, stopped, cancelled
		}
		name := concept(r["medicationCodeableConcept"])
		if name == "" {
			name = str(r, "medicationReference", "display")
		}
		detail := str(r, "dosageInstruction", "text")
		if detail == "" {
			detail = str(r, "dosage", "text")
		}
		since := str(r, "authoredOn")
		if since == "" {
			since = str(r, "dateWritten")
		}
		if name != "" {
			ex.Medications = append(ex.Medications, Item{Name: name, Detail: detail, Since: fhirDate(since)})
		}
	case "Immunization":
		if s := status(r["status"]); s == "not-done" || s == "entered-in-error" || r["wasNotGiven"] == true || r["notGiven"] == true {
			return
		}
		when := str(r, "occurrenceDateTime")
		if when == "" {
			when = str(r, "date")
		}
		if name := concept(r["vaccineCode"]); name != "" {
			ex.Vaccinations = append(ex.Vaccinations, Item{Name: name, Since: fhirDate(when)})
		}
	default:
		a.skipped["hospital "+strings.ToLower(str(r, "resourceType"))]++
	}
}

// gone reports a record that no longer applies: resolved, inactive, in
// remission, refuted or entered in error.
func gone(clinical, verification string) bool {
	switch clinical {
	case "resolved", "inactive", "remission":
		return true
	}
	switch verification {
	case "refuted", "entered-in-error":
		return true
	}
	return false
}

func fhirObservation(a *acc, ex *Export, r obj) {
	switch status(r["status"]) {
	case "cancelled", "entered-in-error":
		return
	}
	when := str(r, "effectiveDateTime")
	if when == "" {
		when = str(r, "effectivePeriod", "start")
	}
	if when == "" {
		when = str(r, "issued")
	}
	if when == "" {
		return
	}
	when = fhirDate(when)
	flag := strings.ToUpper(str(r, "interpretation", "coding", "code"))
	if flag == "" {
		flag = strings.ToUpper(str(r, "interpretation", "text"))
	}
	if flag == "N" || flag == "NORMAL" {
		flag = ""
	}
	add := func(code any, v obj) {
		name := concept(code)
		lc := loinc(code)
		if n, ok := loincNames[lc]; ok {
			name = n
		}
		if name == "" {
			return
		}
		rd := Reading{Name: name, Code: lc, When: when, Flag: flag, FromChart: true,
			RefLow: float(r, "referenceRange", "low", "value"), RefHigh: float(r, "referenceRange", "high", "value")}
		switch {
		case float(v, "valueQuantity", "value") != nil:
			rd.Value = float(v, "valueQuantity", "value")
			rd.Unit = str(v, "valueQuantity", "unit")
			if rd.Unit == "" {
				rd.Unit = str(v, "valueQuantity", "code")
			}
		case str(v, "valueString") != "":
			rd.Text = str(v, "valueString")
		case concept(v["valueCodeableConcept"]) != "":
			rd.Text = concept(v["valueCodeableConcept"])
		default:
			return
		}
		if rd.Text == "" && rd.RefLow == nil && rd.RefHigh == nil {
			rd.Note = str(r, "referenceRange", "text")
		}
		a.done = append(a.done, rd)
		ex.Clinical++
	}
	if comps, ok := r["component"].([]any); ok && len(comps) > 0 {
		for _, c := range comps {
			if m, ok := c.(obj); ok {
				add(m["code"], m)
			}
		}
		return
	}
	add(r["code"], r)
}
