package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNameMatches(t *testing.T) {
	for _, c := range []struct {
		profile, printed string
		want             bool
	}{
		{"Jane Doe", "DOE, JANE M", true},
		{"Jane Doe", "Jane Q. Doe", true},
		{"Jane", "Jane Doe", true},
		{"Jane Doe", "", true},
		{"Jane Doe", "John Doe", false},
		{"Jane Doe", "Jane Smith", false},
		{"Christopher Doe", "Chris Doe", true},
		{"Maria O'Neil", "O'NEIL^MARIA", true},
	} {
		if got := NameMatches(c.profile, c.printed); got != c.want {
			t.Errorf("NameMatches(%q, %q) = %v", c.profile, c.printed, got)
		}
	}
}

func TestParseTidies(t *testing.T) {
	r, err := Parse(`{"kind":"Labs","title":" Lipid panel ","doc_date":"2026-13-01","provider":"","patient_name":"",
		"summary":"s","confidence":"sure","questions":[""],"medications":[{"name":"","detail":"x"}],
		"observations":[{"name":"LDL","value":142,"value_text":"","unit":"mg/dL","ref_low":null,"ref_high":99,"flag":"H","date":"2026-09-15"},
		                {"name":"","value":1,"value_text":"","unit":"","ref_low":null,"ref_high":null,"flag":"","date":""},
		                {"name":"Note","value":null,"value_text":"","unit":"","ref_low":null,"ref_high":null,"flag":"","date":""}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "other" || r.Title != "Lipid panel" || r.DocDate != "" || r.Confidence != "medium" {
		t.Fatalf("tidy: %+v", r)
	}
	if len(r.Observations) != 1 || *r.Observations[0].RefHigh != 99 || len(r.Medications) != 0 || len(r.Questions) != 0 {
		t.Fatalf("lists: %+v", r)
	}
	if _, err := Parse("not json"); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestCheck(t *testing.T) {
	today := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	owner := Person{ID: 1, Name: "Jane Doe", Born: "1980-01-01"}
	others := []Person{{ID: 2, Name: "Sam Doe"}}
	if reasons, _ := Check(Result{Confidence: "high", PatientName: "DOE, JANE", DocDate: "2026-09-15"}, Content{}, owner, others, today); len(reasons) != 0 {
		t.Fatalf("clean result held: %v", reasons)
	}
	reasons, better := Check(Result{Confidence: "high", PatientName: "Sam Doe"}, Content{}, owner, others, today)
	if len(reasons) != 1 || better == nil || better.ID != 2 {
		t.Fatalf("other person's file: %v %v", reasons, better)
	}
	reasons, _ = Check(Result{Confidence: "low", DocDate: "2027-01-01", Questions: []string{"Is the 7 a 1?"}}, Content{Rows: 900, Sampled: true}, owner, nil, today)
	if len(reasons) != 4 {
		t.Fatalf("want low confidence, future date, sampled table and the question: %v", reasons)
	}
}

func TestExtractCSVSamples(t *testing.T) {
	var b strings.Builder
	b.WriteString("time,glucose\n")
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, "2026-10-01T%02d:00,%d\n", i%24, 90+i%40)
	}
	c, err := Extract(context.Background(), "text/csv", "cgm.csv", strings.NewReader(b.String()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Rows != 1000 || !c.Sampled || strings.Count(c.Text, "\n") != maxCSVRows {
		t.Fatalf("rows %d sampled %v lines %d", c.Rows, c.Sampled, strings.Count(c.Text, "\n"))
	}
}

func TestExtractDocx(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Visit note</w:t></w:r></w:p><w:p><w:r><w:t>BP 128/82</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	c, err := Extract(context.Background(), "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "note.docx", &buf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != "Visit note\nBP 128/82" {
		t.Fatalf("text %q", c.Text)
	}
}

func TestExtractUnsupported(t *testing.T) {
	if _, err := Extract(context.Background(), "application/octet-stream", "x.bin", strings.NewReader("x"), t.TempDir()); err != ErrUnsupported {
		t.Fatalf("got %v", err)
	}
}
