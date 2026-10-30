package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/DeGrandis/doc-ish/internal/ollama"
)

// ocrFake answers like DeepSeek-OCR in grounding mode: layout labels, and
// tables as HTML. The second image is a picture with no text.
type ocrFake struct{ calls int }

func (f *ocrFake) Chat(_ context.Context, r ollama.ChatRequest, _ func(string)) (string, []ollama.ToolCall, error) {
	f.calls++
	if f.calls == 2 {
		return "<|ref|>image<|/ref|><|det|>[[0, 0, 999, 999]]<|/det|>", nil, nil
	}
	return `<|ref|>title<|/ref|><|det|>[[52, 40, 600, 70]]<|/det|>
# CUMULATIVE LABORATORY RESULTS

<|ref|>table<|/ref|><|det|>[[50, 90, 950, 400]]<|/det|>
<table><tr><td>Test</td><td>03/10/2025</td><td>09/12/2025</td><td>03/12/2026</td><td>Units</td></tr><tr><td>Ferritin</td><td></td><td>18 L</td><td>41</td><td>ng/mL</td></tr><tr><td>Iron &amp; TIBC</td><td></td><td>38 L</td><td>72</td><td>ug/dL</td></tr></table>`, nil, nil
}

func (f *ocrFake) ChatJSON(context.Context, ollama.ChatRequest, any) (string, error) { return "", nil }

func TestOCR(t *testing.T) {
	cont := Content{PageTexts: []string{"Printed page one with plenty of words on it to count as text for sure here.", "", ""},
		Images: [][]byte{{1}, {2}}, ImagePages: []int{2, 3}, Note: "2 of its 3 pages are scans."}
	var steps []string
	if err := OCR(context.Background(), &ocrFake{}, "deepseek-ocr", &cont, func(w string, d, n int) { steps = append(steps, w) }); err != nil {
		t.Fatal(err)
	}
	page := cont.PageTexts[1]
	if strings.Contains(page, "<|") || strings.Contains(page, "<td>") || !strings.Contains(page, "| Ferritin |  | 18 L | 41 | ng/mL |") ||
		!strings.Contains(page, "Iron & TIBC") {
		t.Fatalf("page 2:\n%s", page)
	}
	// The picture stays a picture, for the vision model.
	if len(cont.Images) != 1 || cont.ImagePages[0] != 3 || cont.PageTexts[2] != "" || len(steps) != 2 {
		t.Fatalf("images %v pages %v steps %v", len(cont.Images), cont.ImagePages, steps)
	}
	if !strings.Contains(cont.Text, "Ferritin") || !strings.Contains(cont.Extractor, "+ocr") || !strings.HasPrefix(cont.Note, "The OCR model read 1 scanned page") {
		t.Fatalf("content: %q %q", cont.Extractor, cont.Note)
	}
	// A Markdown row splits into its cells, so the table logic follows it.
	if cs := cells("| Ferritin |  | 18 L | 41 | ng/mL |"); len(cs) != 4 || !isRow("| Ferritin |  | 18 L | 41 | ng/mL |") ||
		!isHeader("| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units |") {
		t.Fatalf("cells %q", cs)
	}
}

func TestOCRCollapsedTable(t *testing.T) {
	if !collapsed("<table>Glucose98101 H104 Hmg/dL70-99</table>") || collapsed("<table><tr><td>a</td></tr></table>") || collapsed("| a | b |") {
		t.Fatal("collapsed tables")
	}
	if got := cleanOCR("<|im_end|>\n\n## VISIT NOTE\nDate of service: 03/10/2025"); got != "## VISIT NOTE\nDate of service: 03/10/2025" {
		t.Fatalf("%q", got)
	}
}
