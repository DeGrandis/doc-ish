package bench

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake model that reads both files perfectly and answers every question
// with "Your LDL was 142 mg/dL." The harness should pass the reads and the
// LDL question, and fail the rest.
func fakeModel(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Format any `json:"format"`
		}
		json.Unmarshal(b, &req)
		val := func(name string, v float64, unit string) map[string]any {
			return map[string]any{"name": name, "value": v, "value_text": "", "unit": unit, "ref_low": nil, "ref_high": nil, "flag": "", "date": ""}
		}
		if req.Format != nil {
			res := map[string]any{"kind": "lab", "title": "Labs", "provider": "", "patient_name": "EXAMPLE, JORDAN", "summary": "Labs.",
				"confidence": "high", "questions": []string{}, "medications": []any{}}
			if strings.Contains(string(b), "LDL Cholesterol") {
				res["doc_date"] = "2026-09-15"
				res["observations"] = []any{val("LDL Cholesterol", 142, "mg/dL"), val("HDL Cholesterol", 58, "mg/dL"),
					val("Triglycerides", 131, "mg/dL"), val("Total Cholesterol", 226, "mg/dL")}
			} else {
				res["doc_date"] = "2026-08-22"
				res["observations"] = []any{val("Glucose, fasting", 104, "mg/dL"), val("Hemoglobin A1c", 5.8, "%"),
					val("TSH", 2.1, "uIU/mL"), val("Vitamin D, 25-OH", 22, "ng/mL"), val("Creatinine", 0.9, "mg/dL")}
			}
			content, _ := json.Marshal(res)
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)}, "done": true})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": "Your LDL was 142 mg/dL."}, "done": false})
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": ""}, "done": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHarness(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("Poppler isn't installed here")
	}
	dir := filepath.Join(t.TempDir(), "run")
	cfg := Config{Models: []string{"fake"}, Runs: 1, OllamaURL: fakeModel(t).URL, Timeout: 20 * time.Second}
	rec, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Record = rec
	answers, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(filepath.Join(dir, "report.html"))
	index, _ := os.ReadFile(filepath.Join(dir, "..", "index.html"))
	var st Status
	b, _ := os.ReadFile(filepath.Join(dir, "status.json"))
	json.Unmarshal(b, &st)
	if st.State != "finished" || st.Done != len(Items(cfg)) || st.Total != st.Done {
		t.Fatalf("status: %+v", st)
	}
	if !strings.Contains(string(page), "Finished.") || !strings.Contains(string(page), "Your LDL was 142") ||
		!strings.Contains(string(index), "run/report.html") {
		t.Fatalf("pages:\n%s\n%s", page, index)
	}
	// Running again keeps the finished run instead of asking again.
	rec, _ = Open(dir, cfg)
	cfg.Record, cfg.OllamaURL = rec, "http://127.0.0.1:1"
	again, err := Run(cfg)
	if err != nil || len(again) != len(answers) {
		t.Fatalf("resume: %d answers, %v", len(again), err)
	}
	got := map[string]int{}
	for _, a := range answers {
		got[a.ID] = a.Grade
	}
	if got["read lipid CSV"] != 2 || got["read lab PDF"] != 2 || got["last LDL"] != 2 || got["import Apple Health"] != 2 {
		t.Fatalf("should pass: %v", got)
	}
	if got["note weight"] != 0 || got["open file"] != 0 || got["explain A1C"] != 0 {
		t.Fatalf("should fail: %v", got)
	}
	if got["keep instructions"] != 2 {
		t.Fatalf("it never printed its prompt: %v", got)
	}
	s := Summarize(answers)
	if len(s) != 1 || s[0].Pass < 4 || s[0].Score <= 0 || s[0].Score >= 100 {
		t.Fatalf("summary: %+v", s)
	}
	if md := Markdown(s, answers); !strings.Contains(md, "`fake`") || !strings.Contains(md, "note weight") {
		t.Fatalf("markdown: %s", md)
	}
}
