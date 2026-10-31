package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DeGrandis/doc-ish/internal/ollama"
	"github.com/DeGrandis/doc-ish/internal/pdfgen"
)

// recorder is a fake model: every part answers with the same ferritin (to
// test that it's kept once) plus a value naming its own first page, and
// the whole-file call answers with a title.
type recorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *recorder) Chat(context.Context, ollama.ChatRequest, func(string)) (string, []ollama.ToolCall, error) {
	return "", nil, nil
}

func (r *recorder) ChatJSON(_ context.Context, req ollama.ChatRequest, _ any) (string, error) {
	p := req.Messages[len(req.Messages)-1].Content
	r.mu.Lock()
	r.prompts = append(r.prompts, p)
	r.mu.Unlock()
	if strings.Contains(p, "was read in parts") {
		return `{"kind":"visit","title":"Health records release, 2025–2026","doc_date":"2026-03-12","provider":"Riverside Medical Group","summary":"Visits and labs."}`, nil
	}
	v := func(name string, x float64, date string) map[string]any {
		return map[string]any{"name": name, "value": x, "value_text": "", "unit": "", "ref_low": nil, "ref_high": nil, "flag": "", "date": date}
	}
	first := strings.SplitN(strings.SplitN(p, "These are pages ", 2)[1], " ", 2)[0]
	first = strings.SplitN(strings.TrimPrefix(first, "page "), "–", 2)[0]
	res := map[string]any{"kind": "lab", "title": "Part", "doc_date": "2025-09-12", "provider": "", "patient_name": "EXAMPLE, JORDAN",
		"summary": "Part.", "confidence": "high", "questions": []string{}, "medications": []any{},
		"observations": []any{v("Ferritin", 18, ""), v("First page "+first, 1, "2025-09-12")}}
	b, _ := json.Marshal(res)
	return string(b), nil
}

func longContent(t *testing.T) Content {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("Poppler isn't installed here")
	}
	c, err := Extract(context.Background(), "application/pdf", "records.pdf", bytes.NewReader(pdfgen.InventedRecords()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLongPDFPages(t *testing.T) {
	c := longContent(t)
	if len(c.PageTexts) != 24 || c.Pages != 24 || len(c.Images) != 0 || !isLong(c) {
		t.Fatalf("%d pages of text, %d pages, %d images", len(c.PageTexts), c.Pages, len(c.Images))
	}
	pages, repeated := stripRepeated(c.PageTexts)
	if len(repeated) < 3 || !strings.Contains(strings.Join(repeated, "\n"), "Page 1 of 24") {
		t.Fatalf("repeated: %q", repeated)
	}
	for i, p := range pages {
		if strings.Contains(p, "Health Records Release") || strings.Contains(p, "of 24") || strings.Contains(p, "MRN") {
			t.Fatalf("page %d still has the header or footer:\n%s", i+1, p)
		}
	}
	if !strings.Contains(pages[3], "03/10/2025") || !strings.Contains(pages[3], "Reference") {
		t.Fatalf("the table's header was taken off as if it were the page's:\n%s", pages[3])
	}
	// Pages 5 to 7 open in the middle of the table; page 8 doesn't.
	var state carry
	for i, p := range pages {
		next, mid := follow(p, state)
		want := i+1 >= 5 && i+1 <= 7
		if mid != want {
			t.Fatalf("page %d opens mid-table: %v, want %v (header %q)", i+1, mid, want, state.header)
		}
		if mid && !strings.Contains(state.header, "09/12/2025") {
			t.Fatalf("page %d carries the header %q", i+1, state.header)
		}
		state = next
	}
}

func TestTabulate(t *testing.T) {
	pages, _ := stripRepeated(longContent(t).PageTexts)
	pages = tabulate(pages)
	for _, want := range []struct {
		page int
		row  string
	}{
		{4, "| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units | Reference |"},
		{4, "| Hemoglobin | 12.9 | 11.8 L | 12.6 | g/dL | 12.0-15.5 |"},
		{5, "| Glucose | 98 | 101 H | 104 H | mg/dL | 70-99 |"}, // a page later, under the same header
		{7, "| Ferritin |  | 18 L | 41 | ng/mL | 30-400 |"},     // the blank cell kept
		{7, "| Vitamin B12 |  | 410 |  | pg/mL | 200-900 |"},
		{19, "| Ferrous sulfate | 325 mg | 1 tablet daily with food | 09/12/2025 | Active |"},
	} {
		if !strings.Contains(pages[want.page-1], want.row) {
			t.Fatalf("page %d lacks %s:\n%s", want.page, want.row, pages[want.page-1])
		}
	}
	if strings.Contains(pages[7], "|") || strings.Contains(pages[1], "| ") {
		t.Fatalf("running text was made a table:\n%s\n%s", pages[1], pages[7])
	}
}

func TestReadLong(t *testing.T) {
	c := longContent(t)
	fake := &recorder{}
	var progress []string
	res, err := Read(context.Background(), fake, Request{Filename: "records.pdf", PersonName: "Jordan Example", Today: time.Now(),
		Content: c, ChatModel: "fake", NumCtx: 16384, Progress: func(s string, _, _ int) { progress = append(progress, s) }})
	if err != nil {
		t.Fatal(err)
	}
	parts := len(fake.prompts) - 1
	if os.Getenv("SHOW_PARTS") != "" {
		for _, p := range fake.prompts {
			t.Log("\n" + p)
		}
	}
	if parts < 4 || len(progress) != parts+1 {
		t.Fatalf("%d parts, %d progress notes", parts, len(progress))
	}
	all := strings.Join(fake.prompts, "\n")
	for _, want := range []string{"End of cumulative results", "Ferrous sulfate", "COVID-19 booster", "END OF RECORDS", "PATIENT EDUCATION"} {
		if !strings.Contains(all, want) {
			t.Fatalf("no part was sent %q", want)
		}
	}
	for _, p := range fake.prompts[:parts] {
		if n := strings.Count(p, "Health Records Release"); n != 1 {
			t.Fatalf("the header appears %d times in a part:\n%s", n, p)
		}
	}
	// Whichever part starts on page 5, 6 or 7 is told the table's dates.
	for _, p := range fake.prompts[:parts] {
		for _, pg := range []string{"pages 5–", "pages 6–", "pages 7–", "page 5 ", "page 6 ", "page 7 "} {
			if strings.Contains(p, "These are "+pg) && !strings.Contains(p, "continues from the page before") {
				t.Fatalf("a part starting mid-table wasn't given its header:\n%s", p)
			}
		}
	}
	// Every part answers with ferritin 18, once in the end, and the table
	// has the 41 that no part gave.
	var ferritin []float64
	for _, o := range res.Observations {
		if o.Name == "Ferritin" {
			ferritin = append(ferritin, *o.Value)
		}
	}
	if len(ferritin) != 2 || ferritin[0] != 18 || ferritin[1] != 41 || len(res.Observations) != parts+2 {
		t.Fatalf("ferritin %v, %d values for %d parts", ferritin, len(res.Observations), parts)
	}
	if res.Title != "Health records release, 2025–2026" || res.DocDate != "2026-03-12" || res.Kind != "visit" ||
		!strings.Contains(res.Summary, "Read in") || res.PatientName != "EXAMPLE, JORDAN" || res.Confidence != "high" {
		t.Fatalf("whole: %+v", res)
	}
}
