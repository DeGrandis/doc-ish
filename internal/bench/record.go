package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DeGrandis/doc-ish/internal/brand"
)

// A run's folder, written as it goes so its page shows progress:
//
//	meta.json      the plan: models, runs, what's graded
//	status.json    what it's doing now, and how far along it is
//	answers.jsonl  every graded answer, one per line
//	summary.json   the leaderboard, for the index of runs
//	report.html    the page
//
// and index.html in the folder above lists every run side by side.

// Meta is a run's plan.
type Meta struct {
	Started  time.Time `json:"started"`
	Models   []string  `json:"models"`
	Runs     int       `json:"runs"`
	Provider string    `json:"provider"`
	Timeout  float64   `json:"timeout_seconds"`
	Items    []Item    `json:"items"`
}

// Status is what a run is doing.
type Status struct {
	State   string    `json:"state"` // starting, reading, asking, paused, finished, failed
	Model   string    `json:"model,omitempty"`
	ModelN  int       `json:"model_n,omitempty"`
	Run     int       `json:"run,omitempty"`
	Ask     string    `json:"ask,omitempty"`
	Since   time.Time `json:"since"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	ETA     float64   `json:"eta_seconds,omitempty"`
	Updated time.Time `json:"updated"`
	Error   string    `json:"error,omitempty"`
}

// Recorder keeps a run's folder up to date.
type Recorder struct {
	dir     string
	mu      sync.Mutex
	meta    Meta
	status  Status
	answers []Answer
	began   time.Time // this process's start, for the pace
	added   int       // answers this process added
}

// Open starts or resumes the run in dir. Model runs it already finished
// are kept; one that was cut short starts over.
func Open(dir string, cfg Config) (*Recorder, error) {
	cfg = cfg.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	items := Items(cfg)
	r := &Recorder{dir: dir, began: time.Now()}
	r.meta = Meta{Started: time.Now().UTC(), Models: cfg.Models, Runs: cfg.Runs, Provider: cfg.Provider,
		Timeout: cfg.Timeout.Seconds(), Items: items}
	var old Meta
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil && json.Unmarshal(b, &old) == nil && !old.Started.IsZero() {
		r.meta.Started = old.Started
	}
	var prev []Answer
	if f, err := os.Open(filepath.Join(dir, "answers.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var a Answer
			if json.Unmarshal(sc.Bytes(), &a) == nil {
				prev = append(prev, a)
			}
		}
		f.Close()
	}
	count := map[string]int{}
	for _, a := range prev {
		count[runKey(a.Model, a.Run)]++
	}
	for _, a := range prev {
		if count[runKey(a.Model, a.Run)] >= len(items) {
			r.answers = append(r.answers, a)
		}
	}
	var lines []byte
	for _, a := range r.answers {
		b, _ := json.Marshal(a)
		lines = append(append(lines, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "answers.jsonl"), lines, 0o644); err != nil {
		return nil, err
	}
	r.status = Status{State: "starting", Since: time.Now().UTC(), Done: len(r.answers), Total: len(cfg.Models) * cfg.Runs * len(items)}
	r.write()
	return r, nil
}

func runKey(model string, run int) string { return fmt.Sprintf("%s\x00%d", model, run) }

// Has reports whether this model run is already finished.
func (r *Recorder) Has(model string, run int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.answers {
		if a.Model == model && a.Run == run {
			n++
		}
	}
	return n >= len(r.meta.Items)
}

// Answers is everything recorded so far.
func (r *Recorder) Answers() []Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Answer{}, r.answers...)
}

// Now says what the run is doing.
func (r *Recorder) Now(state, model string, n, run int, ask string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.State, r.status.Model, r.status.ModelN, r.status.Run, r.status.Ask = state, model, n, run, ask
	r.status.Since = time.Now().UTC()
	r.write()
}

// Add records a graded answer.
func (r *Recorder) Add(a Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers = append(r.answers, a)
	r.added++
	r.status.Done = len(r.answers)
	if f, err := os.OpenFile(filepath.Join(r.dir, "answers.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		b, _ := json.Marshal(a)
		f.Write(append(b, '\n'))
		f.Close()
	}
	r.write()
}

// Pause marks the run stopped part way; running it again picks it up.
func (r *Recorder) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.State, r.status.ETA = "paused", 0
	r.write()
}

// Finish marks the run done, or failed with err.
func (r *Recorder) Finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.State, r.status.Ask, r.status.ETA = "finished", "", 0
	if err != nil {
		r.status.State, r.status.Error = "failed", err.Error()
	}
	r.write()
}

// write rewrites the run's files and the index; r.mu is held.
func (r *Recorder) write() {
	st := &r.status
	st.Updated = time.Now().UTC()
	if r.added > 0 && (st.State == "reading" || st.State == "asking") {
		per := time.Since(r.began).Seconds() / float64(r.added)
		st.ETA = per * float64(st.Total-st.Done)
	}
	sums := Summarize(r.answers)
	writeJSON(filepath.Join(r.dir, "meta.json"), r.meta)
	writeJSON(filepath.Join(r.dir, "status.json"), st)
	writeJSON(filepath.Join(r.dir, "summary.json"), map[string]any{"meta": r.meta, "status": st, "leaderboard": sums})
	writeFile(filepath.Join(r.dir, "report.html"), Report(r.meta, *st, r.answers))
	WriteIndex(filepath.Dir(r.dir))
}

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	writeFile(path, string(b))
}

// writeFile replaces path whole, so a page never reads half a file.
func writeFile(path, s string) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(s), 0o644) == nil {
		os.Rename(tmp, path)
	}
}

var esc = html.EscapeString

const css = `:root{--bg:#0b0b0a;--bg2:#141412;--fg:#f3f2ea;--muted:#9b998f;--line:rgba(255,255,255,.09);--accent:#ff8a6a;--ok:#b9f04a;--mid:#f5c04a;--bad:#ff6b5e}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1400px;margin:0 auto;padding:28px 16px 60px}h1{margin:0 0 4px}h2{margin:34px 0 10px}.muted{color:var(--muted)}a{color:var(--accent)}
table{border-collapse:collapse;width:100%}th,td{padding:6px 8px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top;white-space:nowrap}
th{color:var(--muted);font-weight:600;font-size:12px;text-transform:uppercase}.scroll{overflow-x:auto}td.wrap{white-space:normal;min-width:260px}
td.pass{color:var(--ok);text-align:center}td.partial{color:var(--mid);text-align:center}td.fail{color:var(--bad);text-align:center}td.none{text-align:center;color:var(--muted)}
.bar{display:inline-block;height:8px;background:var(--accent);border-radius:4px;vertical-align:middle;margin-right:6px}
.status{background:var(--bg2);border:1px solid var(--line);border-radius:12px;padding:12px 16px;margin:16px 0}
.prog{height:8px;background:rgba(255,255,255,.08);border-radius:4px;overflow:hidden;margin-top:8px}.prog i{display:block;height:100%;background:var(--accent)}
details{background:var(--bg2);border:1px solid var(--line);border-radius:10px;padding:8px 12px;margin:6px 0}summary{cursor:pointer}
pre{white-space:pre-wrap;word-break:break-word;margin:6px 0;font:13px/1.45 ui-monospace,Consolas,monospace;color:#dcdad0}
.tag{display:inline-block;padding:1px 7px;border-radius:999px;border:1px solid var(--line);font-size:12px;margin:2px 4px 2px 0}
.tag.ok{border-color:var(--ok);color:var(--ok)}.tag.no{border-color:var(--bad);color:var(--bad)}.stale{color:var(--mid)}
.up{color:var(--ok)}.down{color:var(--bad)}`

// live reloads the page when status.json changes, keeping open answers
// and the scroll position; opened as a file, it reloads every 30 s.
const live = `<script>(function(){
var S=JSON.parse(document.getElementById('st').textContent),key='bench:'+location.pathname;
try{var k=JSON.parse(sessionStorage.getItem(key)||'null');if(k){k.open.forEach(function(id){var d=document.getElementById(id);if(d)d.open=true});scrollTo(0,k.y)}}catch(e){}
function save(){try{sessionStorage.setItem(key,JSON.stringify({y:scrollY,open:[].map.call(document.querySelectorAll('details[open]'),function(d){return d.id})}))}catch(e){}}
function reload(){save();location.reload()}
var going=S.state=='starting'||S.state=='reading'||S.state=='asking',limit=(S.limit||300)*1000;
function tick(){var f=document.getElementById('for');if(f&&going)f.textContent=Math.round((Date.now()-Date.parse(S.since))/1000)+' s';
var s=document.getElementById('stale');if(s&&going&&Date.now()-Date.parse(S.updated)>limit)s.hidden=false}
tick();setInterval(tick,1000);if(!going)return;
if(location.protocol=='file:'){setTimeout(reload,30000);return}
setInterval(function(){fetch(S.url,{cache:'no-store'}).then(function(r){return r.json()}).then(function(n){if(n.updated!=S.updated)reload()}).catch(function(){})},4000)})()</script>`

func dur(sec float64) string {
	switch {
	case sec < 90:
		return fmt.Sprintf("%.0f s", sec)
	case sec < 90*60:
		return fmt.Sprintf("%.0f min", sec/60)
	}
	return fmt.Sprintf("%.1f h", sec/3600)
}

func tone(frac float64) string {
	switch {
	case frac >= 0.999:
		return "pass"
	case frac <= 0.001:
		return "fail"
	}
	return "partial"
}

var gradeName = []string{"fail", "partial", "pass"}

// Report is a run's page.
func Report(m Meta, st Status, answers []Answer) string {
	sums := Summarize(answers)
	var h strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&h, f, a...) }
	stj, _ := json.Marshal(map[string]any{"state": st.State, "since": st.Since, "updated": st.Updated,
		"limit": 2*m.Timeout + 120, "url": "status.json"})
	w("<!doctype html><html><head><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'>"+
		"<title>%s benchmark</title><style>%s</style></head><body><main><p class='muted'><a href='../index.html'>All runs</a></p>", esc(brand.Name), css)
	nf := 0
	for _, it := range m.Items {
		if strings.HasPrefix(it.Ask, "(drop") {
			nf++
		}
	}
	nq := len(m.Items) - nf
	w("<h1>%s benchmark</h1><p class='muted'>%d files to read and %d questions, each run on a fresh copy of an invented household "+
		"(the real app, in-process); %d run(s) per model on %s. Pass = 2 points, partial = 1, fail or error = 0. Each answer gets %s "+
		"(reading both files, twice that). Started %s; page written %s.</p>",
		esc(brand.Name), nf, nq, m.Runs, esc(m.Provider), dur(m.Timeout),
		m.Started.Local().Format("2006-01-02 15:04"), time.Now().Format("2006-01-02 15:04:05"))

	// What it's doing now.
	var line string
	where := fmt.Sprintf("<b>%s</b> (model %d of %d), run %d of %d", esc(st.Model), st.ModelN, len(m.Models), st.Run, m.Runs)
	switch st.State {
	case "reading":
		line = "Now: " + where + ": starting the app and reading the two files, for <span id='for'></span>"
	case "asking":
		line = fmt.Sprintf("Now: %s: “%s” for <span id='for'></span>", where, esc(st.Ask))
	case "paused":
		line = "Paused at " + where + ". Run the same command again to pick up where it stopped; finished runs are kept."
	case "finished":
		line = "Finished."
	case "failed":
		line = "Stopped with an error: " + esc(st.Error)
	default:
		line = "Starting."
	}
	pct := 0.0
	if st.Total > 0 {
		pct = 100 * float64(st.Done) / float64(st.Total)
	}
	eta := ""
	if st.ETA > 0 {
		eta = " · roughly " + dur(st.ETA) + " to go at the pace so far"
	}
	w("<div class='status'>%s<br><span class='muted'>%d of %d answers%s</span>"+
		"<span class='stale' id='stale' hidden> · No progress for a while: the run may have stopped.</span>"+
		"<div class='prog'><i style='width:%.1f%%'></i></div></div>", line, st.Done, st.Total, eta, pct)

	// The leaderboard.
	w("<h2>Leaderboard</h2><div class='scroll'><table><tr><th>Model</th><th>Score</th><th>Range across runs</th><th>Same grade every run</th>" +
		"<th>Pass</th><th>Partial</th><th>Fail</th><th>Answers</th><th>Median s/answer</th><th>Errors</th></tr>")
	seen := map[string]bool{}
	for _, s := range sums {
		seen[s.Model] = true
		rng := "one run so far"
		if len(s.RunScores) > 1 {
			lo, hi := s.RunScores[0], s.RunScores[0]
			for _, v := range s.RunScores {
				lo, hi = min(lo, v), max(hi, v)
			}
			rng = fmt.Sprintf("%g–%g%%", lo, hi)
		}
		steady := "—"
		if len(s.RunScores) > 1 {
			steady = fmt.Sprintf("%g%% of items", s.Steady)
		}
		w("<tr><td><b>%s</b></td><td><span class='bar' style='width:%.0fpx'></span>%g%%</td><td>%s</td><td>%s</td>"+
			"<td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%g</td><td>%d</td></tr>",
			esc(s.Model), s.Score, s.Score, rng, steady, s.Pass, s.Partial, s.Fail, s.Pass+s.Partial+s.Fail, s.Median, s.Errors)
	}
	var waiting []string
	for _, mo := range m.Models {
		if !seen[mo] {
			waiting = append(waiting, mo)
		}
	}
	if len(waiting) > 0 {
		w("<tr><td colspan='10' class='muted'>Still to run: %s</td></tr>", esc(strings.Join(waiting, ", ")))
	}
	w("</table></div><p class='muted'>Score: points earned of points possible, all runs together. Range: the lowest and highest " +
		"single run. Same grade every run: share of items graded the same way each time. Median: the typical answer's seconds " +
		"(a read counts the time to read both files). Errors: answers that failed with an error or ran out of time.</p>")

	// Every item, every model.
	type key struct{ model, id string }
	by := map[key][]Answer{}
	for _, a := range answers {
		by[key{a.Model, a.ID}] = append(by[key{a.Model, a.ID}], a)
	}
	for k := range by {
		sort.Slice(by[k], func(i, j int) bool { return by[k][i].Run < by[k][j].Run })
	}
	var ms []string
	for _, s := range sums {
		ms = append(ms, s.Model)
	}
	w("<h2>Every item</h2><p class='muted'>Passes out of runs: green every time, red never, yellow sometimes or partial. " +
		"Hover for each run's grade and time.</p><div class='scroll'><table><tr><th>Item</th><th>Asked</th><th>Must</th>")
	for _, mo := range ms {
		w("<th>%s</th>", esc(mo))
	}
	w("</tr>")
	for _, it := range m.Items {
		w("<tr><td><b>%s</b></td><td class='wrap'>%s</td><td class='wrap muted'>%s</td>", esc(it.ID), esc(it.Ask), esc(it.Must))
		for _, mo := range ms {
			as := by[key{mo, it.ID}]
			if len(as) == 0 {
				w("<td class='none'>·</td>")
				continue
			}
			passes, pts := 0, 0
			var tips []string
			for _, a := range as {
				pts += a.Grade
				if a.Grade == 2 {
					passes++
				}
				tips = append(tips, fmt.Sprintf("%s %gs", gradeName[a.Grade], a.Seconds))
			}
			w("<td class='%s' title='%s'>%d/%d</td>", tone(float64(pts)/float64(2*len(as))), esc(strings.Join(tips, " · ")), passes, len(as))
		}
		w("</tr>")
	}
	w("</table></div>")

	// What each model said.
	w("<h2>Answers</h2>")
	for i, it := range m.Items {
		w("<details id='item%d'><summary><b>%s</b> %s <span class='muted'>· must %s</span></summary>", i, esc(it.ID), esc(it.Ask), esc(it.Must))
		asked := false
		for _, mo := range ms {
			for _, a := range by[key{mo, it.ID}] {
				asked = true
				tags := ""
				for _, why := range a.Missed {
					tags += "<span class='tag no'>" + esc(why) + "</span>"
				}
				if a.Grade == 2 {
					tags += "<span class='tag ok'>every check</span>"
				}
				tools := "no tools"
				if len(a.Tools) > 0 {
					tools = "tools: " + strings.Join(a.Tools, ", ")
				}
				text := a.Text
				if a.Error != "" {
					text = strings.TrimSpace(text + "\n(error: " + a.Error + ")")
				}
				if text == "" {
					text = "(no answer)"
				}
				w("<p><b>%s</b> <span class='muted'>run %d · %s · %gs · %s</span><br>%s</p><pre>%s</pre>",
					esc(mo), a.Run, gradeName[a.Grade], a.Seconds, esc(tools), tags, esc(text))
			}
		}
		if !asked {
			w("<p class='muted'>Not asked yet.</p>")
		}
		w("</details>")
	}
	w("</main><script type='application/json' id='st'>%s</script>%s</body></html>", stj, live)
	return h.String()
}

// WriteIndex writes index.html in results: every run in it, linked, with
// each model's score side by side.
func WriteIndex(results string) {
	ents, err := os.ReadDir(results)
	if err != nil {
		return
	}
	type run struct {
		name  string
		meta  Meta
		st    Status
		board []Summary
	}
	var runs []run
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(results, e.Name(), "summary.json"))
		if err != nil {
			continue
		}
		var s struct {
			Meta        Meta      `json:"meta"`
			Status      Status    `json:"status"`
			Leaderboard []Summary `json:"leaderboard"`
		}
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		runs = append(runs, run{e.Name(), s.Meta, s.Status, s.Leaderboard})
	}
	if len(runs) == 0 {
		return
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].meta.Started.Before(runs[j].meta.Started) })
	var h strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&h, f, a...) }
	w("<!doctype html><html><head><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'>"+
		"<meta http-equiv=refresh content=30><title>%s benchmark runs</title><style>%s</style></head><body><main><h1>%s benchmark runs</h1><ul>",
		esc(brand.Name), css, esc(brand.Name))
	var models []string
	have := map[string]bool{}
	for i := len(runs) - 1; i >= 0; i-- {
		for _, s := range runs[i].board {
			if !have[s.Model] {
				have[s.Model] = true
				models = append(models, s.Model)
			}
		}
	}
	for _, r := range runs {
		state := r.st.State
		if state != "finished" {
			state = fmt.Sprintf("%s, %d of %d answers", state, r.st.Done, r.st.Total)
		}
		w("<li><a href='%s/report.html'>%s</a> <span class='muted'>started %s, %d models × %d runs, %s</span></li>",
			esc(r.name), esc(r.name), r.meta.Started.Local().Format("2006-01-02 15:04"), len(r.meta.Models), r.meta.Runs, esc(state))
	}
	w("</ul><h2>Scores side by side</h2><div class='scroll'><table><tr><th>Model</th>")
	for _, r := range runs {
		w("<th>%s</th>", esc(r.name))
	}
	if len(runs) > 1 {
		w("<th>Change, %s → %s</th>", esc(runs[0].name), esc(runs[len(runs)-1].name))
	}
	w("</tr>")
	for _, mo := range models {
		w("<tr><td><b>%s</b></td>", esc(mo))
		var first, last *Summary
		for i, r := range runs {
			var s *Summary
			for j := range r.board {
				if r.board[j].Model == mo {
					s = &r.board[j]
				}
			}
			if i == 0 {
				first = s
			}
			if i == len(runs)-1 {
				last = s
			}
			if s == nil {
				w("<td class='muted'>—</td>")
				continue
			}
			w("<td>%g%% <span class='muted'>· %gs</span></td>", s.Score, s.Median)
		}
		if len(runs) > 1 {
			if first != nil && last != nil {
				d := round1(last.Score - first.Score)
				cls := ""
				if d > 0 {
					cls = "up"
				} else if d < 0 {
					cls = "down"
				}
				w("<td class='%s'>%+g</td>", cls, d)
			} else {
				w("<td class='muted'>—</td>")
			}
		}
		w("</tr>")
	}
	w("</table></div><p class='muted'>Each run's own page has the details. Seconds: median per answer. A run still going shows its score so far.</p></main></body></html>")
	writeFile(filepath.Join(results, "index.html"), h.String())
}
