package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DeGrandis/mymedical/internal/pdfgen"
)

// TestTesseract reads the invented release's lab table as a scan (pages
// 4 to 7, rendered at 130 dpi like a fax) and checks that a row on the
// last page, which has no header and a blank first cell, keeps its
// values under their dates.
func TestTesseract(t *testing.T) {
	if tesseractPath() == "" {
		t.Skip("tesseract isn't installed")
	}
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("poppler-utils aren't installed")
	}
	dir := t.TempDir()
	pdf := filepath.Join(dir, "r.pdf")
	os.WriteFile(pdf, pdfgen.InventedRecords(), 0o600)
	if err := exec.Command("pdftoppm", "-r", "130", "-jpeg", "-f", "4", "-l", "7", pdf, filepath.Join(dir, "pg")).Run(); err != nil {
		t.Fatal(err)
	}
	cont := Content{PageTexts: make([]string, 4), Extractor: "pdf-images"}
	for i, n := range []string{"04", "05", "06", "07"} {
		b, err := os.ReadFile(filepath.Join(dir, "pg-"+n+".jpg"))
		if err != nil {
			t.Fatal(err)
		}
		cont.Images = append(cont.Images, b)
		cont.ImagePages = append(cont.ImagePages, i+1)
	}
	if err := Tesseract(context.Background(), &cont, nil); err != nil {
		t.Fatal(err)
	}
	if len(cont.Images) != 0 || cont.Extractor != "pdf-images+tesseract" {
		t.Fatalf("%d images left, %q", len(cont.Images), cont.Extractor)
	}
	pages, _ := stripRepeated(cont.PageTexts)
	pages = tabulate(pages)
	for _, want := range []string{
		`\| Ferritin \|\s+\| 18 L \| 41 \|`,          // a blank first cell, on a page with no header
		`\| Vitamin B12 \|\s+\| 410 \|\s+\|`,         // and a blank last one
		`\| Hemoglobin \| 12.9 \| 11.8 L \| 12.6 \|`, // a flag read as part of its number, split off
		`\| Sodium \| 139 \| 140 \| 138 \|`,
	} {
		if !regexp.MustCompile(want).MatchString(strings.Join(pages, "\n")) {
			t.Errorf("no %s in:\n%s", want, strings.Join(pages, "\n=====\n"))
		}
	}
	if strings.Contains(strings.Join(pages, "\n"), "MRN") {
		t.Error("the page header wasn't taken off")
	}
}

func TestLayout(t *testing.T) {
	// Two pages at 10 px a letter: words of a sentence close up, columns
	// stay where they are on both pages.
	pages := [][]word{
		{{text: "Test", line: 0, left: 0, width: 40, conf: 95}, {text: "03/10/2025", line: 0, left: 200, width: 100, conf: 95},
			{text: "09/12/2025", line: 0, left: 400, width: 100, conf: 95}},
		{{text: "Iron", line: 0, left: 0, width: 40, conf: 95}, {text: "38", line: 0, left: 400, width: 20, conf: 95},
			{text: "Seen", line: 1, left: 0, width: 40, top: 100, conf: 95}, {text: "today", line: 1, left: 55, width: 50, top: 100, conf: 95}},
	}
	got := layout(pages)
	if got[0] != "Test                03/10/2025          09/12/2025" || !strings.HasPrefix(got[1], "Iron                                    38\n") ||
		!strings.Contains(got[1], "Seen today") {
		t.Fatalf("%q", got)
	}
}

func TestNearly(t *testing.T) {
	a := normLine("Patient: EXAMPLE, JORDAN                 DOB: 06/02/1979             MRN: 00482913")
	b := normLine("Patient: EXAMPLE, ] ORDAN                 DOB: 06/02/1979             MRN: 00482913")
	if !nearly(a, b) || nearly("vitals:", "visits:") || nearly(a, normLine("Patient education: iron in your diet and how to take it")) {
		t.Fatal("nearly")
	}
}

func TestShortRows(t *testing.T) {
	pages := []string{
		"| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units |\n| WBC | 6.2 | 5.8 | 6.0 | x10^3/uL |",
		"| Ferritin | 18 L | 41 | ng/mL |\n| Iron |  | 38 L | 72 | ug/dL |",
		"Seen for follow-up of tiredness, which has been worse over the summer, and labs today show a low value.\n| Ferritin | 18 L | 41 |",
	}
	if got := shortRows(pages); len(got) != 1 || got[0] != 2 {
		t.Fatalf("%v", got)
	}
}
