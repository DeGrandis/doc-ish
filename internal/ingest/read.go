package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/ollama"
)

// Result is what the reader made of a file.
type Result struct {
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	DocDate      string   `json:"doc_date"`
	Provider     string   `json:"provider"`
	PatientName  string   `json:"patient_name"`
	Summary      string   `json:"summary"`
	Observations []Value  `json:"observations"`
	Medications  []Med    `json:"medications"`
	Confidence   string   `json:"confidence"` // high, medium, low
	Questions    []string `json:"questions"`
}

// Value is one result or measurement as printed.
type Value struct {
	Name      string   `json:"name"`
	Value     *float64 `json:"value"`
	ValueText string   `json:"value_text"`
	Unit      string   `json:"unit"`
	RefLow    *float64 `json:"ref_low"`
	RefHigh   *float64 `json:"ref_high"`
	Flag      string   `json:"flag"`
	Date      string   `json:"date"`
}

// Med is a medication mentioned in the file.
type Med struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Kinds are the kinds of file the reader sorts into.
var Kinds = []string{"lab", "imaging", "visit", "prescription", "vaccination", "bill", "device", "other"}

var nullableNumber = map[string]any{"type": []string{"number", "null"}}

// schema is the JSON the model must answer with.
var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"kind":         map[string]any{"type": "string", "enum": Kinds},
		"title":        map[string]any{"type": "string"},
		"doc_date":     map[string]any{"type": "string"},
		"provider":     map[string]any{"type": "string"},
		"patient_name": map[string]any{"type": "string"},
		"summary":      map[string]any{"type": "string"},
		"observations": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"}, "value": nullableNumber, "value_text": map[string]any{"type": "string"},
				"unit": map[string]any{"type": "string"}, "ref_low": nullableNumber, "ref_high": nullableNumber,
				"flag": map[string]any{"type": "string"}, "date": map[string]any{"type": "string"},
			},
			"required": []string{"name", "value", "value_text", "unit", "ref_low", "ref_high", "flag", "date"},
		}},
		"medications": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}, "detail": map[string]any{"type": "string"}},
			"required":   []string{"name", "detail"},
		}},
		"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		"questions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required": []string{"kind", "title", "doc_date", "provider", "patient_name", "summary", "observations", "medications", "confidence", "questions"},
}

const instructions = `You read medical files for a family's private records app and describe them as JSON.

- kind: lab (test results), imaging (X-ray, CT, MRI, ultrasound and their reports), visit (visit notes, discharge summaries, referrals), prescription, vaccination, bill (bills, insurance, EOBs), device (exports from a scale, blood pressure cuff, glucose meter, watch), or other.
- title: a short, specific name a person would recognize, like "Lipid panel" or "Knee MRI report". Not the file name.
- doc_date: when the test, visit or image happened, as YYYY-MM-DD. Empty if not shown. Don't use today's date or the file name unless nothing else gives a date.
- provider: the clinic, lab or doctor, if shown.
- patient_name: the patient's name exactly as printed. Empty if not shown.
- summary: two or three plain sentences about what it is and what it shows.
- observations: every measured result or vital sign, one entry each, copied exactly: the test name, value (a number, or null with value_text for results like "Negative" or "<0.5"), unit, the reference range as ref_low/ref_high when printed (null when not), flag as printed (H, L, A, High, Low…, else empty), and date (YYYY-MM-DD) when it differs from doc_date. Never compute or guess values. For a table of readings, one entry per reading.
- medications: any medications named, with dose and directions in detail.
- confidence: high if the file is clear and you read it fully; medium if parts were unclear; low if you can't tell what it is.
- questions: only doubts about reading the file itself: a part you couldn't read, a value or name you weren't sure you read right, a table you couldn't line up. Results that are high or low are NOT questions (the app flags them from the ranges), and neither is a missing range. Usually empty.
If the file isn't medical, say so in summary, use kind "other" and confidence "low".`

// Request is everything the reader needs about one file.
type Request struct {
	Filename    string
	PersonName  string
	Today       time.Time
	Content     Content
	ChatModel   string // for text
	VisionModel string // for images
	NumCtx      int
}

// Read asks the model about the file and returns its tidied answer. A
// DICOM file is described from its tags without a model.
func Read(ctx context.Context, c *ollama.Client, req Request) (Result, error) {
	if req.Content.Dicom != nil {
		return dicomResult(req.Content.Dicom), nil
	}
	text := strings.TrimSpace(req.Content.Text)
	images := req.Content.Images
	if text == "" && len(images) == 0 {
		return Result{}, errors.New("there's no text or picture in it to read")
	}

	model := req.ChatModel
	if len(images) > 0 {
		model = req.VisionModel
		if model == "" {
			return Result{}, errors.New("pick a model for images and scans in Settings")
		}
	}
	if model == "" {
		return Result{}, errors.New("pick a chat model in Settings")
	}

	// Leave room for the instructions and the answer; about three
	// characters to a token.
	budget := max((req.NumCtx-4500)*3, 4000)
	cut := false
	if len(text) > budget {
		text, cut = strings.ToValidUTF8(text[:budget], ""), true
	}

	var user strings.Builder
	fmt.Fprintf(&user, "File name: %s\nWhose records: %s\nToday: %s\n", req.Filename, req.PersonName, req.Today.Format("2006-01-02"))
	if req.Content.Note != "" {
		fmt.Fprintf(&user, "Note: %s\n", req.Content.Note)
	}
	if cut {
		user.WriteString("Note: the file is long; only its beginning is below.\n")
	}
	if text != "" {
		fmt.Fprintf(&user, "\nThe file's text:\n<<<\n%s\n>>>\n", text)
	}
	if len(images) > 0 {
		fmt.Fprintf(&user, "\nThe file is shown in the %d attached image(s).\n", len(images))
	}

	msg := ollama.Message{Role: "user", Content: user.String()}
	for _, img := range images {
		msg.Images = append(msg.Images, base64.StdEncoding.EncodeToString(img))
	}
	raw, err := c.ChatJSON(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: []ollama.Message{{Role: "system", Content: instructions}, msg},
		NumCtx:   req.NumCtx,
	}, schema)
	if err != nil {
		return Result{}, err
	}
	res, err := Parse(raw)
	if err != nil {
		return Result{}, err
	}
	if cut {
		res.Questions = append(res.Questions, "Only the beginning of this long file was read.")
	}
	return res, nil
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Parse reads the model's JSON and tidies it: known kinds only, real
// dates only, no empty or non-finite values.
func Parse(raw string) (Result, error) {
	var r Result
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &r); err != nil {
		return r, errors.New("the model's answer wasn't readable; try a different model, or read it again")
	}
	r.Kind = strings.ToLower(strings.TrimSpace(r.Kind))
	known := false
	for _, k := range Kinds {
		known = known || k == r.Kind
	}
	if !known {
		r.Kind = "other"
	}
	r.Title = clip(r.Title, 120)
	r.Provider = clip(r.Provider, 160)
	r.PatientName = clip(r.PatientName, 120)
	r.Summary = clip(r.Summary, 1200)
	r.DocDate = cleanDate(r.DocDate)
	switch r.Confidence {
	case "high", "medium", "low":
	default:
		r.Confidence = "medium"
	}
	vals := r.Observations[:0]
	for _, v := range r.Observations {
		v.Name = clip(v.Name, 120)
		v.ValueText = clip(v.ValueText, 200)
		v.Unit = clip(v.Unit, 40)
		v.Flag = clip(v.Flag, 20)
		v.Date = cleanDate(v.Date)
		v.Value, v.RefLow, v.RefHigh = finite(v.Value), finite(v.RefLow), finite(v.RefHigh)
		if v.Name == "" || (v.Value == nil && v.ValueText == "") {
			continue
		}
		vals = append(vals, v)
		if len(vals) == 500 {
			break
		}
	}
	r.Observations = vals
	meds := r.Medications[:0]
	for _, m := range r.Medications {
		if m.Name = clip(m.Name, 120); m.Name != "" {
			m.Detail = clip(m.Detail, 300)
			meds = append(meds, m)
		}
	}
	r.Medications = meds
	qs := r.Questions[:0]
	for _, q := range r.Questions {
		if q = clip(q, 300); q != "" {
			qs = append(qs, q)
		}
	}
	r.Questions = qs
	return r, nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func cleanDate(s string) string {
	s = strings.TrimSpace(s)
	if !isoDate.MatchString(s) {
		return ""
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return ""
	}
	return s
}

func finite(f *float64) *float64 {
	if f == nil || math.IsNaN(*f) || math.IsInf(*f, 0) {
		return nil
	}
	return f
}
