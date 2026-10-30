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

	"github.com/DeGrandis/doc-ish/internal/ollama"
)

// Result is what the reader made of a file.
type Result struct {
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	DocDate      string     `json:"doc_date"`
	Provider     string     `json:"provider"`
	PatientName  string     `json:"patient_name"`
	Summary      string     `json:"summary"`
	Observations []Value    `json:"observations"`
	Medications  []Med      `json:"medications"`
	FollowUps    []FollowUp `json:"follow_ups"`
	Confidence   string     `json:"confidence"` // high, medium, low
	Questions    []string   `json:"questions"`
	// A long file with several records in it (a records release: visit
	// notes, lab reports, letters) has a section for each, and its values
	// and follow-ups say which section they came from.
	Sections []Section `json:"sections,omitempty"`
	// Pages is what a part of a long file says each of its pages is.
	Pages []PageLabel `json:"pages,omitempty"`
}

// Section is one record inside a long file: pages First to Last.
type Section struct {
	First    int    `json:"first"`
	Last     int    `json:"last"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Date     string `json:"date"`
	Provider string `json:"provider"`
	Summary  string `json:"summary"`
}

// PageLabel is the record one page belongs to.
type PageLabel struct {
	Page      int    `json:"page"`
	NewRecord bool   `json:"new_record"` // a record starts on this page
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Date      string `json:"date"`
	Provider  string `json:"provider"`
	About     string `json:"about"`
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
	Section   int      `json:"section,omitempty"` // in a long file, its section (from 1)
}

// Med is a medication mentioned in the file.
type Med struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// FollowUp is something the file says should happen later: a recheck,
// a repeat scan, a follow-up visit.
type FollowUp struct {
	What string `json:"what"`
	When string `json:"when"` // as written: "in about six months"
	Due  string `json:"due"`  // YYYY-MM-DD: printed, or worked out from When
	From string `json:"from"` // the date of the note that said it (the app fills it in)
	// In a long file, its section (from 1).
	Section int `json:"section,omitempty"`
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
		"follow_ups": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"what": map[string]any{"type": "string"}, "when": map[string]any{"type": "string"}, "due": map[string]any{"type": "string"}},
			"required":   []string{"what", "when", "due"},
		}},
		"confidence": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		"questions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required": []string{"kind", "title", "doc_date", "provider", "patient_name", "summary", "observations", "medications", "follow_ups", "confidence", "questions"},
}

const instructions = `You read medical files for a family's private records app and describe them as JSON.

- kind: lab (test results), imaging (X-ray, CT, MRI, ultrasound and their reports), visit (visit notes, discharge summaries, referrals), prescription, vaccination, bill (bills, insurance, EOBs), device (exports from a scale, blood pressure cuff, glucose meter, watch), or other.
- title: a short, specific name a person would recognize, like "Lipid panel" or "Knee MRI report". Not the file name.
- doc_date: when the test, visit or image happened, as YYYY-MM-DD. Empty if not shown. Don't use today's date or the file name unless nothing else gives a date.
- provider: the clinic, lab or doctor, if shown.
- patient_name: the patient's name exactly as printed. Empty if not shown.
- summary: two or three plain sentences about what it is and what it shows.
- observations: every measured result or vital sign, one entry each, copied exactly: the test name, value (a number, or null with value_text for results like "Negative" or "<0.5"), unit, the reference range as ref_low/ref_high when printed (null when not), flag as printed (H, L, A, High, Low…, else empty), and date (YYYY-MM-DD) when it differs from doc_date. Never compute or guess values. For a table of readings, one entry per reading. When a table has a column for each date (a cumulative report: "Test | 03/10/2025 | 09/12/2025 | …"), every filled cell is its own observation, dated by its column: three dates mean up to three entries for each test, not one. An empty cell means no result that day.
- medications: any medications named, with dose and directions in detail.
- follow_ups: what the file says should happen later: a recheck or repeat test, a follow-up visit, a scan to repeat, a referral, a vaccine due. what: short, like "Repeat chest CT" or "Recheck blood count and iron". when: the timing exactly as written ("in about six months", "in 12 months", "next fall"). due: YYYY-MM-DD only when a date is printed, else empty. Not medications to keep taking, and not things already done. Usually empty.
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
	// DescribeOnly asks what the file is without copying its values: for a
	// long table, which an import recipe reads instead.
	DescribeOnly bool
	// Progress hears which pages of a long file are being read.
	Progress Progress
}

// describeRows is how much of a long table is shown when only describing it.
const describeRows = 30

// Read asks the model about the file and returns its tidied answer. A
// DICOM file is described from its tags without a model.
func Read(ctx context.Context, c ollama.Chatter, req Request) (Result, error) {
	if req.Content.Dicom != nil {
		return dicomResult(req.Content.Dicom), nil
	}
	text := strings.TrimSpace(req.Content.Text)
	images := req.Content.Images
	if text == "" && len(images) == 0 {
		return Result{}, errors.New("there's no text or picture in it to read")
	}
	if isLong(req.Content) && !req.DescribeOnly {
		if req.ChatModel == "" && req.VisionModel == "" {
			return Result{}, errors.New("pick a chat model in Settings")
		}
		return readLong(ctx, c, req)
	}

	// A PDF's tables, written out cell by cell (see tables.go).
	if len(req.Content.PageTexts) > 0 && len(images) == 0 {
		text = strings.TrimSpace(strings.Join(tabulate(req.Content.PageTexts), "\n\n"))
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
	if req.DescribeOnly {
		lines := strings.SplitN(text, "\n", describeRows+2)
		if len(lines) > describeRows+1 {
			lines = lines[:describeRows+1]
		}
		text = strings.Join(lines, "\n")
		fmt.Fprintf(&user, "Note: this is a long table (%d rows); only its first rows are below. Describe the file, but leave observations empty: the app reads every row another way.\n", req.Content.Rows)
	} else if cut {
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
	if req.DescribeOnly {
		res.Observations = nil
	}
	if cut && !req.DescribeOnly {
		res.Questions = append(res.Questions, "Only the beginning of this long file was read.")
	}
	return res, nil
}

var isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

var errUnreadable = errors.New("the model's answer wasn't readable; try a different model, or read it again")

// Parse reads the model's JSON and tidies it: known kinds only, real
// dates only, no empty or non-finite values.
func Parse(raw string) (Result, error) {
	var r Result
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &r); err != nil {
		return r, errUnreadable
	}
	r.Kind = knownKind(r.Kind)
	labels := r.Pages[:0]
	for _, l := range r.Pages {
		if l.Page > 0 {
			l.Kind, l.Title, l.Date = knownKind(l.Kind), clip(l.Title, 120), cleanDate(l.Date)
			l.Provider, l.About = clip(l.Provider, 160), clip(l.About, 300)
			labels = append(labels, l)
		}
	}
	r.Pages = labels
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
	fs := r.FollowUps[:0]
	for _, f := range r.FollowUps {
		if f.What = clip(f.What, 160); f.What != "" {
			f.When, f.Due, f.From = clip(f.When, 80), cleanDate(f.Due), cleanDate(f.From)
			fs = append(fs, f)
		}
		if len(fs) == 20 {
			break
		}
	}
	r.FollowUps = fs
	qs := r.Questions[:0]
	for _, q := range r.Questions {
		if q = clip(q, 300); q != "" {
			qs = append(qs, q)
		}
	}
	r.Questions = qs
	return r, nil
}

func knownKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, known := range Kinds {
		if k == known {
			return k
		}
	}
	return "other"
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
