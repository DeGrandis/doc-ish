// Package bench measures how well a model does this app's job: reading
// files and answering about them. Each run starts the real app (the same
// code, prompts and tools people use) on a fresh invented household,
// drops invented files for the reader, asks a fixed set of questions, and
// grades every answer by rules: values that must appear, tools that must
// be used, changes that must really be saved.
package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DeGrandis/doc-ish/internal/app"
	"github.com/DeGrandis/doc-ish/web"
)

// Config says which models to run and where.
type Config struct {
	Models    []string
	Runs      int
	Provider  string // ollama (default) or remote
	OllamaURL string
	RemoteURL string
	RemoteKey string
	NumCtx    int
	Reference string // an existing reference.db (with drug labels) to copy in; optional
	OCRModel  string // a model for reading scans; adds a scanned file to read
	Timeout   time.Duration
	Log       func(string)
	Record    *Recorder // writes progress and the report as it goes; optional
}

func (cfg Config) withDefaults() Config {
	if cfg.Runs < 1 {
		cfg.Runs = 1
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.NumCtx == 0 {
		cfg.NumCtx = 16384
	}
	if cfg.Provider == "" {
		cfg.Provider = "ollama"
	}
	return cfg
}

// Item is one graded thing in a run: a file to read or a question.
type Item struct {
	ID   string `json:"id"`
	Ask  string `json:"ask"`
	Must string `json:"must"`
}

// Answer is one graded question.
type Answer struct {
	Model   string   `json:"model"`
	Run     int      `json:"run"`
	ID      string   `json:"id"`
	Ask     string   `json:"ask"`
	Grade   int      `json:"grade"` // 2 pass, 1 partial, 0 fail
	Missed  []string `json:"missed,omitempty"`
	Seconds float64  `json:"seconds"`
	Text    string   `json:"text"`
	Tools   []string `json:"tools,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// session is one run's app and signed-in client.
type session struct {
	url    string
	c      *http.Client
	docs   map[string]int64 // fixture name -> document id
	events []map[string]any // the last answer's events
}

func (s *session) call(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return resp.StatusCode, nil
}

// start makes a fresh app with the household and its files, set to model.
func start(cfg Config, model string) (*session, func(), error) {
	dir, err := os.MkdirTemp("", "docish-bench-*")
	if err != nil {
		return nil, nil, err
	}
	if cfg.Reference != "" {
		if b, err := os.ReadFile(cfg.Reference); err == nil {
			os.WriteFile(filepath.Join(dir, "reference.db"), b, 0o600)
		}
	}
	a, err := app.New(app.Config{DataDir: dir, Version: "bench", Web: web.Static()})
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	srv := httptest.NewServer(a.Handler())
	stop := func() { srv.Close(); a.Close(); os.RemoveAll(dir) }
	jar, _ := cookiejar.New(nil)
	s := &session{url: srv.URL, c: &http.Client{Jar: jar}, docs: map[string]int64{}}
	steps := []func() error{
		func() error {
			_, err := s.call("POST", "/api/setup", map[string]string{"password": "bench password", "name": patient}, nil)
			return err
		},
		func() error {
			_, err := s.call("PUT", "/api/people/1", map[string]any{"name": patient, "profile": profile}, nil)
			return err
		},
		func() error {
			for _, w := range weights {
				if _, err := s.call("POST", "/api/people/1/observations", map[string]any{"name": "Weight", "value": w.value, "unit": "lb", "observed": w.date}, nil); err != nil {
					return err
				}
			}
			return nil
		},
		func() error {
			set := map[string]any{"provider": "ollama", "ollama_url": cfg.OllamaURL, "chat_model": model, "vision_model": model, "ocr_model": cfg.OCRModel, "num_ctx": cfg.NumCtx}
			if cfg.Provider == "remote" {
				set = map[string]any{"provider": "remote", "remote_url": cfg.RemoteURL, "remote_key": cfg.RemoteKey, "remote_model": model,
					"remote_vision_model": model, "consent": "I accept my data leaking to a provider", "num_ctx": cfg.NumCtx}
			}
			_, err := s.call("PUT", "/api/settings", set, nil)
			return err
		},
	}
	for _, f := range steps {
		if err := f(); err != nil {
			stop()
			return nil, nil, err
		}
	}
	return s, stop, nil
}

// upload adds fixture files and returns their document ids.
func (s *session) upload(files map[string][]byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, b := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(b)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", s.url+"/api/people/1/documents", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := s.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var res []struct {
		Filename string `json:"filename"`
		Document struct {
			ID int64 `json:"id"`
		} `json:"document"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	for _, r := range res {
		s.docs[r.Filename] = r.Document.ID
	}
	return nil
}

// ask sends one question in a new chat and returns the answer's text and
// the lookups it made.
func (s *session) ask(q string, viewing int64, timeout time.Duration) (string, []string, error) {
	b, _ := json.Marshal(map[string]any{"message": q, "viewing": viewing})
	c := *s.c
	c.Timeout = timeout
	resp, err := c.Post(s.url+"/api/people/1/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	var text strings.Builder
	var tools []string
	s.events = nil
	dec := json.NewDecoder(resp.Body)
	for {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			break
		}
		s.events = append(s.events, ev)
		if t, ok := ev["t"].(string); ok {
			text.WriteString(t)
		}
		if t, ok := ev["tool"].(map[string]any); ok {
			tools = append(tools, fmt.Sprint(t["name"]))
		}
		if e, ok := ev["error"].(string); ok {
			return text.String(), tools, fmt.Errorf("%s", e)
		}
	}
	return strings.TrimSpace(text.String()), tools, nil
}

// event reports whether the last answer sent an event of this kind
// (chart, open, change).
func (s *session) event(kind string) bool {
	for _, ev := range s.events {
		if _, ok := ev[kind]; ok {
			return true
		}
	}
	return false
}

// Run runs every model cfg.Runs times and returns every graded answer.
// With cfg.Record, runs it already has are kept rather than run again.
func Run(cfg Config) ([]Answer, error) {
	cfg = cfg.withDefaults()
	logf := func(f string, a ...any) {
		if cfg.Log != nil {
			cfg.Log(fmt.Sprintf(f, a...))
		}
	}
	rec := cfg.Record
	var out []Answer
	if rec != nil {
		out = rec.Answers()
	}
	add := func(a Answer) {
		out = append(out, a)
		if rec != nil {
			rec.Add(a)
		}
	}
	now := func(state, model string, n, run int, item string) {
		if rec != nil {
			rec.Now(state, model, n, run, item)
		}
	}
	for n, model := range cfg.Models {
		for run := 1; run <= cfg.Runs; run++ {
			if rec != nil && rec.Has(model, run) {
				logf("%s, run %d of %d: already done", model, run, cfg.Runs)
				continue
			}
			logf("%s, run %d of %d", model, run, cfg.Runs)
			now("reading", model, n+1, run, "")
			s, stop, err := start(cfg, model)
			if err != nil {
				err = fmt.Errorf("%s: %w", model, err)
				if rec != nil {
					rec.Finish(err)
				}
				return out, err
			}
			for _, a := range readFiles(s, cfg) {
				a.Model, a.Run = model, run
				add(a)
				logf("  %-26s %s %5.1fs %s", a.ID, mark(a.Grade), a.Seconds, strings.Join(a.Missed, "; "))
			}
			for _, q := range questions(cfg) {
				now("asking", model, n+1, run, q.Ask)
				a := q.run(s, cfg.Timeout)
				a.Model, a.Run = model, run
				add(a)
				logf("  %-26s %s %5.1fs %s%s", a.ID, mark(a.Grade), a.Seconds, strings.Join(a.Missed, "; "), a.Error)
			}
			stop()
		}
		unload(cfg, model)
	}
	if rec != nil {
		rec.Finish(nil)
	}
	return out, nil
}

// unload frees the GPU for the next model (Ollama only; it would swap
// anyway, but this makes the memory free at once and after the last one).
func unload(cfg Config, model string) {
	if cfg.Provider != "ollama" {
		return
	}
	b, _ := json.Marshal(map[string]any{"model": model, "keep_alive": 0})
	c := &http.Client{Timeout: 30 * time.Second}
	if resp, err := c.Post(strings.TrimRight(cfg.OllamaURL, "/")+"/api/generate", "application/json", bytes.NewReader(b)); err == nil {
		resp.Body.Close()
	}
}

func mark(g int) string { return []string{"fail   ", "partial", "pass   "}[g] }

// Summary is one model's totals.
type Summary struct {
	Model     string    `json:"model"`
	Score     float64   `json:"score"` // points earned of points possible
	Pass      int       `json:"pass"`
	Partial   int       `json:"partial"`
	Fail      int       `json:"fail"`
	Median    float64   `json:"median_seconds"`
	RunScores []float64 `json:"run_scores"` // each run's score, in run order
	Steady    float64   `json:"steady"`     // share of items graded the same in every run, in %
	Errors    int       `json:"errors"`     // errors and timeouts
}

// Summarize totals the answers per model, best first.
func Summarize(answers []Answer) []Summary {
	by := map[string]*Summary{}
	times := map[string][]float64{}
	runs := map[string]map[int][2]int{}     // model -> run -> points, possible
	grades := map[string]map[string][]int{} // model -> item -> grades
	var order []string
	for _, a := range answers {
		s := by[a.Model]
		if s == nil {
			s = &Summary{Model: a.Model}
			by[a.Model] = s
			order = append(order, a.Model)
			runs[a.Model] = map[int][2]int{}
			grades[a.Model] = map[string][]int{}
		}
		switch a.Grade {
		case 2:
			s.Pass++
		case 1:
			s.Partial++
		default:
			s.Fail++
		}
		if a.Error != "" {
			s.Errors++
		}
		times[a.Model] = append(times[a.Model], a.Seconds)
		r := runs[a.Model][a.Run]
		runs[a.Model][a.Run] = [2]int{r[0] + a.Grade, r[1] + 2}
		grades[a.Model][a.ID] = append(grades[a.Model][a.ID], a.Grade)
	}
	var out []Summary
	for _, m := range order {
		s := by[m]
		n := s.Pass + s.Partial + s.Fail
		s.Score = round1(float64(2*s.Pass+s.Partial) / float64(2*n) * 100)
		t := times[m]
		sort.Float64s(t)
		s.Median = t[len(t)/2]
		var nums []int
		for r := range runs[m] {
			nums = append(nums, r)
		}
		sort.Ints(nums)
		for _, r := range nums {
			p := runs[m][r]
			s.RunScores = append(s.RunScores, round1(float64(p[0])/float64(p[1])*100))
		}
		same := 0
		for _, gs := range grades[m] {
			if len(gs) > 0 && allSame(gs) {
				same++
			}
		}
		s.Steady = round1(float64(same) / float64(len(grades[m])) * 100)
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Median < out[j].Median
	})
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// allSame reports whether every grade is the same.
func allSame(gs []int) bool {
	for _, g := range gs {
		if g != gs[0] {
			return false
		}
	}
	return true
}

// Markdown is the results table for docs/benchmark.md.
func Markdown(sums []Summary, answers []Answer) string {
	var b strings.Builder
	b.WriteString("| Model | Score | Pass / partial / fail | Typical answer |\n|---|---:|---:|---:|\n")
	for _, s := range sums {
		fmt.Fprintf(&b, "| `%s` | %.1f%% | %d / %d / %d | %.1f s |\n", s.Model, s.Score, s.Pass, s.Partial, s.Fail, s.Median)
	}
	b.WriteString("\nWhat each model missed:\n\n")
	for _, s := range sums {
		var misses []string
		for _, a := range answers {
			if a.Model == s.Model && a.Grade < 2 {
				why := strings.Join(a.Missed, "; ")
				if a.Error != "" {
					why = a.Error
				}
				misses = append(misses, fmt.Sprintf("%s (run %d): %s", a.ID, a.Run, why))
			}
		}
		if len(misses) == 0 {
			misses = []string{"nothing"}
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", s.Model, strings.Join(misses, "; "))
	}
	return b.String()
}
