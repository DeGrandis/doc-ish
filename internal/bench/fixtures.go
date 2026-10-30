package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"fmt"
	"github.com/DeGrandis/mymedical/internal/pdfgen"
	"strings"
)

// The invented household every run starts from. None of it is anyone's.

const patient = "Jordan Example"

var profile = map[string]any{
	"born": "1979-06-02", "sex": "Female", "height": "5'6\"", "blood_type": "A+",
	"allergies":   []map[string]string{{"name": "Penicillin", "detail": "hives"}},
	"medications": []map[string]string{{"name": "Lisinopril", "detail": "10 mg daily", "since": "2023"}},
	"conditions":  []map[string]string{{"name": "High blood pressure", "since": "2023"}},
}

// weights are earlier weigh-ins, oldest first.
var weights = []struct {
	date  string
	value float64
}{{"2026-06-01", 172.0}, {"2026-07-01", 170.4}, {"2026-08-01", 168.8}, {"2026-09-01", 167.5}}

// lipidCSV is a lab export the reader must copy exactly.
const lipidCSV = `Patient,Jordan Example
Collected,2026-09-15
Lab,Example Clinical Laboratory

Test,Result,Units,Reference range,Flag
LDL Cholesterol,142,mg/dL,0-99,H
HDL Cholesterol,58,mg/dL,40-,
Triglycerides,131,mg/dL,0-149,
Total Cholesterol,226,mg/dL,0-199,H
`

// metabolicText is a lab report as plain text.
const metabolicText = `EXAMPLE CLINICAL LABORATORY
Patient: EXAMPLE, JORDAN     DOB: 06/02/1979
Collected: 08/22/2026

TEST                 RESULT   FLAG  UNITS    REFERENCE
Glucose, fasting     104      H     mg/dL    70-99
Hemoglobin A1c       5.8      H     %        4.0-5.6
TSH                  2.1            uIU/mL   0.4-4.0
Vitamin D, 25-OH     22       L     ng/mL    30-100
Creatinine           0.9            mg/dL    0.6-1.1
`

// labPDF is the same report as a text PDF, for the PDF path.
func labPDF() []byte {
	var ops strings.Builder
	ops.WriteString("BT /F1 11 Tf ")
	y := 760
	for _, l := range strings.Split(strings.TrimSpace(metabolicText), "\n") {
		l = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(l)
		fmt.Fprintf(&ops, "1 0 0 1 50 %d Tm (%s) Tj ", y, l)
		y -= 18
	}
	ops.WriteString("ET")
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", ops.Len(), ops.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>",
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return []byte(b.String())
}

var scan struct {
	once sync.Once
	pdf  []byte
	err  error
}

// scannedRecords is the invented records release as a scan: its pages
// rendered to images (with Poppler) and put back in a PDF with no text.
func scannedRecords() ([]byte, error) {
	scan.once.Do(func() {
		dir, err := os.MkdirTemp("", "bench-scan-*")
		if err != nil {
			scan.err = err
			return
		}
		defer os.RemoveAll(dir)
		pdf := filepath.Join(dir, "r.pdf")
		os.WriteFile(pdf, pdfgen.InventedRecords(), 0o600)
		if scan.err = exec.Command("pdftoppm", "-r", "130", "-jpeg", pdf, filepath.Join(dir, "pg")).Run(); scan.err != nil {
			return
		}
		files, _ := filepath.Glob(filepath.Join(dir, "pg-*.jpg"))
		sort.Strings(files)
		var pages [][]byte
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				scan.err = err
				return
			}
			pages = append(pages, b)
		}
		scan.pdf, scan.err = pdfgen.Scanned(pages)
	})
	return scan.pdf, scan.err
}
