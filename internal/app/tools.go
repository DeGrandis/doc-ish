package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/doc-ish/internal/blobs"
	"github.com/DeGrandis/doc-ish/internal/ingest"
	"github.com/DeGrandis/doc-ish/internal/ollama"
	"github.com/DeGrandis/doc-ish/internal/reference"
	"github.com/DeGrandis/doc-ish/internal/store"
)

// The assistant's tools. Lookups are read-only. note_value and
// update_profile write, because what they save is the person's own words
// ("weight 182 today", "I'm allergic to penicillin"); each change shows in
// the chat with an Undo button and in Activity.

// msgExtra is what an answer carried besides its text, kept with the
// message so a reloaded chat shows the same charts and buttons.
type msgExtra struct {
	Tools   []toolUse   `json:"tools,omitempty"`
	Charts  []chartSpec `json:"charts,omitempty"`
	Opened  []int64     `json:"opened,omitempty"`
	Changes []change    `json:"changes,omitempty"`
}

type toolUse struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Doc   int64  `json:"doc,omitempty"`
	URL   string `json:"url,omitempty"` // a source to read more, like a MedlinePlus page
}

type change struct {
	ActivityID int64  `json:"activity_id"`
	Summary    string `json:"summary"`
}

type chartSpec struct {
	Title  string        `json:"title"`
	Series []chartSeries `json:"series"`
}

type chartSeries struct {
	Name    string       `json:"name"`
	Unit    string       `json:"unit"`
	RefLow  *float64     `json:"ref_low,omitempty"`
	RefHigh *float64     `json:"ref_high,omitempty"`
	Points  []chartPoint `json:"points"`
}

type chartPoint struct {
	Date  string  `json:"d"`
	Value float64 `json:"v"`
	Out   bool    `json:"out,omitempty"`
}

// toolEnv is one chat turn's view of the records.
type toolEnv struct {
	st    *store.Store
	files *blobs.Store
	set   Settings
	ref   *reference.Ref // may be nil
	pid   int64
	today time.Time
	emit  func(any)
	extra *msgExtra
}

var str = map[string]any{"type": "string"}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func describe(s string, desc string) map[string]any {
	return map[string]any{"type": s, "description": desc}
}

var profileFields = []string{"born", "sex", "height", "blood_type", "lifestyle", "emergency_contact", "insurance", "notes",
	"conditions", "surgeries", "allergies", "medications", "family_history", "vaccinations", "providers"}

var assistantTools = []ollama.Tool{
	{Name: "search_files", Description: "Find the person's files by words in them, kind or date. Returns each file's id, date, title and summary.",
		Parameters: obj(map[string]any{
			"query": describe("string", "words to look for; empty for the most recent files"),
			"kind":  map[string]any{"type": "string", "enum": append([]string{""}, ingest.Kinds...)},
			"from":  describe("string", "YYYY-MM-DD"), "to": describe("string", "YYYY-MM-DD"),
		})},
	{Name: "read_file", Description: "Read one file in full: what it is, every value, medications named, and its text.",
		Parameters: obj(map[string]any{"id": describe("integer", "the file id from search_files or the file list")}, "id")},
	{Name: "open_file", Description: "Show a file to the user beside the chat, when they ask to see, pull up or open one.",
		Parameters: obj(map[string]any{"id": describe("integer", "the file id")}, "id")},
	{Name: "list_measurements", Description: "Every kind of value on record (weight, lab tests, blood pressure…) with how many readings, the latest and the date range.",
		Parameters: obj(map[string]any{})},
	{Name: "get_values", Description: "Every reading of one measurement over time, oldest first. The name can be partial (\"cholesterol\" finds LDL and HDL).",
		Parameters: obj(map[string]any{"name": str, "from": describe("string", "YYYY-MM-DD"), "to": describe("string", "YYYY-MM-DD")}, "name")},
	{Name: "chart", Description: "Draw a chart of one or more measurements over time for the user, when they ask how something is trending or for a graph.",
		Parameters: obj(map[string]any{
			"names": map[string]any{"type": "array", "items": str, "description": "measurement names, up to 4"},
			"from":  describe("string", "YYYY-MM-DD"), "to": describe("string", "YYYY-MM-DD"), "title": str,
		}, "names")},
	{Name: "explain", Description: "Look up a condition, symptom, test or medical term in MedlinePlus (the National Library of Medicine's plain-language reference). Use it before explaining what something means, and base the explanation on it.",
		Parameters: obj(map[string]any{"topic": describe("string", "the condition, test or term, like \"A1C\", \"high cholesterol\", \"vitamin D deficiency\"")}, "topic")},
	{Name: "lookup_drug", Description: "Look up a medication by brand or generic name: its active ingredient, brand names, strengths, and from its FDA label what it's for, boxed warning, contraindications and warnings.",
		Parameters: obj(map[string]any{"name": str}, "name")},
	{Name: "drug_interactions", Description: "Check whether the FDA labels of two or more medications mention each other in their drug interactions sections. Include every medication the person takes when asked about interactions.",
		Parameters: obj(map[string]any{"drugs": map[string]any{"type": "array", "items": str, "description": "medication names, two or more"}}, "drugs")},
	{Name: "describe_image", Description: "Look at an imaging study (X-ray, CT, MRI, ultrasound) with the vision model: what it shows and anything that stands out, read together with its report when one is linked. Use it when asked what an image shows.",
		Parameters: obj(map[string]any{"id": describe("integer", "the imaging study's file id")}, "id")},
	{Name: "lab_test", Description: "Look up a lab test by name: its standard (LOINC) name and units when available, and what MedlinePlus says about it.",
		Parameters: obj(map[string]any{"name": str}, "name")},
	{Name: "note_value", Description: "Save a measurement the user tells you: weight, blood pressure, glucose, temperature, a symptom score. Use the same name as existing measurements. For blood pressure save two: \"Blood pressure systolic\" and \"Blood pressure diastolic\" in mmHg.",
		Parameters: obj(map[string]any{
			"name":       str,
			"value":      map[string]any{"type": []string{"number", "null"}},
			"value_text": describe("string", "for a value that isn't a number, else empty"),
			"unit":       str,
			"date":       describe("string", "YYYY-MM-DD; today if not said"),
			"note":       str,
		}, "name", "value", "unit")},
	{Name: "add_follow_up", Description: "Add something to what's coming up for the person: a recheck, appointment, scan or vaccine they mention or ask to be reminded of.",
		Parameters: obj(map[string]any{
			"what": describe("string", "short: \"Repeat chest CT\", \"Flu shot\""),
			"due":  describe("string", "YYYY-MM-DD when a date is said, else empty"),
			"when": describe("string", "the timing as said, when it isn't a date: \"in three months\""),
		}, "what")},
	{Name: "set_follow_up", Description: "Mark one of the follow-ups listed in the context done or set aside (dismissed), open it again, or change its due date, when the user says so.",
		Parameters: obj(map[string]any{
			"id":     describe("integer", "the follow-up's id, from [f:N] in the context"),
			"status": map[string]any{"type": "string", "enum": []string{"done", "dismissed", "open", ""}},
			"due":    describe("string", "a new due date, YYYY-MM-DD, or empty to leave it"),
		}, "id")},
	{Name: "update_profile", Description: "Save something about the person to their profile: add or remove an allergy, condition, medication, surgery, family history item, vaccination or doctor, or set born (YYYY-MM-DD), sex, height, blood type, lifestyle, emergency contact, insurance or notes. Weight goes to note_value instead.",
		Parameters: obj(map[string]any{
			"field":  map[string]any{"type": "string", "enum": profileFields},
			"action": map[string]any{"type": "string", "enum": []string{"add", "remove", "set"}},
			"value":  describe("string", "for set"),
			"name":   describe("string", "the item, for add and remove (\"Penicillin\", \"Lisinopril\"); for family_history, the condition (\"Type 2 diabetes\")"),
			"detail": describe("string", "for add: the reaction, the dose, or for family_history who had it (\"father, in his 50s\")"),
			"since":  describe("string", "for add: when it started or happened"),
		}, "field", "action")},
}

// run executes one tool call and returns what the model reads back.
func (e *toolEnv) run(call ollama.ToolCall) string {
	var args map[string]any
	if len(call.Function.Arguments) > 0 {
		// Some models send the arguments as a JSON string.
		var s string
		if json.Unmarshal(call.Function.Arguments, &s) == nil {
			json.Unmarshal([]byte(s), &args)
		} else {
			json.Unmarshal(call.Function.Arguments, &args)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	out, err := e.dispatch(call.Function.Name, args)
	if err != nil {
		return "Error: " + err.Error()
	}
	return out
}

func (e *toolEnv) dispatch(name string, a map[string]any) (string, error) {
	switch name {
	case "search_files":
		return e.searchFiles(sArg(a, "query"), sArg(a, "kind"), sArg(a, "from"), sArg(a, "to"))
	case "read_file":
		return e.readFile(iArg(a, "id"))
	case "open_file":
		return e.openFile(iArg(a, "id"))
	case "list_measurements":
		return e.listMeasurements()
	case "get_values":
		return e.getValues(sArg(a, "name"), sArg(a, "from"), sArg(a, "to"))
	case "chart":
		var names []string
		switch v := a["names"].(type) {
		case []any:
			for _, n := range v {
				if s, ok := n.(string); ok {
					names = append(names, s)
				}
			}
		case string:
			names = strings.Split(v, ",")
		}
		return e.chart(names, sArg(a, "from"), sArg(a, "to"), sArg(a, "title"))
	case "note_value":
		return e.noteValue(a)
	case "explain":
		return e.explain(sArg(a, "topic"))
	case "lookup_drug":
		return e.lookupDrug(sArg(a, "name"))
	case "drug_interactions":
		return e.drugInteractions(strList(a["drugs"]))
	case "lab_test":
		return e.labTest(sArg(a, "name"))
	case "describe_image":
		return e.describeImage(iArg(a, "id"))
	case "add_follow_up":
		return e.addFollowUp(sArg(a, "what"), sArg(a, "due"), sArg(a, "when"))
	case "set_follow_up":
		return e.setFollowUp(iArg(a, "id"), sArg(a, "status"), sArg(a, "due"))
	case "update_profile":
		return e.updateProfile(sArg(a, "field"), sArg(a, "action"), sArg(a, "value"), sArg(a, "name"), sArg(a, "detail"), sArg(a, "since"))
	}
	return "", fmt.Errorf("there's no tool called %q", name)
}

// strList reads a list of strings, or one comma-separated string.
func strList(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, n := range x {
			if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(x, ",") {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

func sArg(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func iArg(a map[string]any, k string) int64 {
	switch v := a[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.Trim(strings.TrimSpace(v), "[]docf:"), 10, 64)
		return n
	}
	return 0
}

// noteLink shows a lookup with a link to its source.
func (e *toolEnv) noteLink(name, label, url string) {
	t := toolUse{Name: name, Label: label, URL: url}
	e.extra.Tools = append(e.extra.Tools, t)
	e.emit(map[string]any{"tool": t})
}

func (e *toolEnv) note(name, label string, doc int64) {
	t := toolUse{Name: name, Label: label, Doc: doc}
	e.extra.Tools = append(e.extra.Tools, t)
	e.emit(map[string]any{"tool": t})
}

// ---------- files ----------

func docDate(d store.Document) string {
	if d.DocDate != "" {
		return d.DocDate
	}
	return d.Added[:10]
}

func docLine(d store.Document) string {
	title := d.Title
	if title == "" {
		title = d.Filename
	}
	s := fmt.Sprintf("[doc:%d] %s %s", d.ID, docDate(d), title)
	var bits []string
	if d.Kind != "" {
		bits = append(bits, d.Kind)
	}
	if d.Provider != "" {
		bits = append(bits, d.Provider)
	}
	switch d.Status {
	case store.StatusNew, store.StatusReading:
		bits = append(bits, "not read yet")
	case store.StatusReview:
		bits = append(bits, "waiting for the user to check it")
	case store.StatusFailed:
		bits = append(bits, "couldn't be read")
	}
	if len(bits) > 0 {
		s += " (" + strings.Join(bits, ", ") + ")"
	}
	if d.Summary != "" {
		sum := d.Summary
		if len([]rune(sum)) > 200 {
			sum = string([]rune(sum)[:200]) + "…"
		}
		s += ": " + sum
	}
	return s
}

func (e *toolEnv) searchFiles(q, kind, from, to string) (string, error) {
	docs, err := e.st.Documents(e.pid, q, 300)
	if err != nil {
		return "", err
	}
	list := func(docs []store.Document) []string {
		var lines []string
		for _, d := range docs {
			dd := docDate(d)
			if (kind != "" && d.Kind != kind) || (from != "" && dd < from) || (to != "" && dd > to) {
				continue
			}
			lines = append(lines, docLine(d))
			if len(lines) == 25 {
				break
			}
		}
		return lines
	}
	lines := list(docs)
	if words := strings.Fields(q); len(lines) == 0 && len(words) > 1 {
		// A model asks in phrases ("knee MRI report 2019"): rather than
		// leave it guessing at other words, show what comes closest.
		if docs, err = e.closestFiles(words); err != nil {
			return "", err
		}
		if lines = list(docs); len(lines) > 0 {
			lines = append([]string{"No file has all of those words. The closest:"}, lines...)
		}
	}
	label := "Searched files"
	if q != "" {
		label += ": " + q
	}
	e.note("search_files", label, 0)
	if len(lines) == 0 {
		return "No files match.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// closestFiles lists the files with any of the words, most words first.
func (e *toolEnv) closestFiles(words []string) ([]store.Document, error) {
	hits := map[int64]int{}
	var found []store.Document
	for _, w := range words {
		docs, err := e.st.Documents(e.pid, w, 300)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if hits[d.ID]++; hits[d.ID] == 1 {
				found = append(found, d)
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return hits[found[i].ID] > hits[found[j].ID] })
	return found, nil
}

func (e *toolEnv) ownDoc(id int64) (store.Document, error) {
	d, err := e.st.Document(id)
	if err != nil || d.PersonID != e.pid {
		return d, fmt.Errorf("there's no file %d on this person's records", id)
	}
	return d, nil
}

const fileTextLimit = 6000

func (e *toolEnv) readFile(id int64) (string, error) {
	d, err := e.ownDoc(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(docLine(d) + "\n")
	if d.Status == store.StatusReview && d.ReviewReason != "" {
		b.WriteString("Waiting for the user to check it because: " + strings.ReplaceAll(d.ReviewReason, "\n", "; ") + "\n")
	}
	room := e.toolRoom()
	if secs, _ := e.st.DocumentSections(id); len(secs) > 0 {
		b.WriteString("Records inside it:\n")
		var lines []string
		for _, s := range secs {
			lines = append(lines, "- "+sectionLine(s))
		}
		if n := within(&b, lines, room/4); n > 0 {
			fmt.Fprintf(&b, "- …and %d more\n", n)
		}
	}
	if vals, _ := e.st.DocumentObservations(id); len(vals) > 0 {
		b.WriteString("Values:\n")
		if n := within(&b, fileValues(vals), room/3); n > 0 {
			fmt.Fprintf(&b, "- …and %d more; get_values finds any by name\n", n)
		}
	}
	var res ingest.Result
	if json.Unmarshal([]byte(d.Extracted), &res) == nil && len(res.Medications) > 0 {
		b.WriteString("Medications named:\n")
		for _, m := range res.Medications {
			b.WriteString("- " + strings.TrimSpace(m.Name+" "+m.Detail) + "\n")
		}
	}
	if t := strings.TrimSpace(d.Text); t != "" {
		short, cut := ollama.Clip(strings.ToValidUTF8(t[:min(len(t), fileTextLimit)], ""), room/3)
		if cut || len(t) > fileTextLimit {
			short += "\n[…the rest is cut]"
		}
		b.WriteString("Text:\n" + short + "\n")
	}
	e.note("read_file", "Read: "+titleOf(d), d.ID)
	return b.String(), nil
}

// toolRoom is the most one tool's answer can take of the model's context.
func (e *toolEnv) toolRoom() int { return contextRoom(e.set.NumCtx) / 3 }

// fileValues lists a file's values for the assistant, what's out of range
// first and then the newest, so those survive when a long file is cut.
func fileValues(vals []store.Observation) []string {
	vals = slices.Clone(vals)
	sort.SliceStable(vals, func(i, j int) bool {
		if a, b := vals[i].OutOfRange(), vals[j].OutOfRange(); a != b {
			return a
		}
		return vals[i].Observed > vals[j].Observed
	})
	lines := make([]string, len(vals))
	for i, o := range vals {
		lines[i] = "- " + obsLine(o)
	}
	return lines
}

// sectionLine is one record inside a long file, for the assistant.
func sectionLine(s store.Section) string {
	line := fmt.Sprintf("pages %d–%d: %s, “%s”, %s", s.FirstPage, s.LastPage, s.Kind, s.Title, or(s.Date, "no date"))
	if s.Provider != "" {
		line += ", " + s.Provider
	}
	if s.Summary != "" {
		line += ". " + s.Summary
	}
	return line
}

func titleOf(d store.Document) string {
	if d.Title != "" {
		return d.Title
	}
	return d.Filename
}

func (e *toolEnv) openFile(id int64) (string, error) {
	d, err := e.ownDoc(id)
	if err != nil {
		return "", err
	}
	e.extra.Opened = append(e.extra.Opened, id)
	e.emit(map[string]any{"open": id})
	e.note("open_file", "Opened: "+titleOf(d), d.ID)
	return fmt.Sprintf("Opened %s beside the chat. The user can see it now.\n%s", titleOf(d), docLine(d)), nil
}

// ---------- values ----------

func obsLine(o store.Observation) string {
	v := o.ValueText
	if o.Value != nil {
		v = strconv.FormatFloat(*o.Value, 'f', -1, 64)
	}
	s := strings.TrimSpace(fmt.Sprintf("%s %s: %s %s", o.Observed, o.Name, v, o.Unit))
	if o.RefLow != nil || o.RefHigh != nil {
		s += fmt.Sprintf(" (range %s–%s)", fnum(o.RefLow), fnum(o.RefHigh))
	}
	if o.OutOfRange() {
		s += " OUT OF RANGE"
		if o.Flag != "" {
			s += " " + o.Flag
		}
	}
	if o.DocumentID != nil {
		s += fmt.Sprintf(" [doc:%d]", *o.DocumentID)
	} else if o.Source != "" {
		s += " (" + o.Source + ")"
	}
	if o.Note != "" {
		s += " note: " + o.Note
	}
	return s
}

func (e *toolEnv) listMeasurements() (string, error) {
	ms, err := e.st.Measurements(e.pid)
	if err != nil {
		return "", err
	}
	sort.Slice(ms, func(i, j int) bool { return strings.ToLower(ms[i].Name) < strings.ToLower(ms[j].Name) })
	e.note("list_measurements", "Listed measurements", 0)
	if len(ms) == 0 {
		return "Nothing measured on record yet.", nil
	}
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "- %s: %d reading%s from %s to %s; latest %s\n", m.Name, m.Count, plural(m.Count), m.First, m.Latest.Observed, obsLine(m.Latest))
	}
	if ollama.Tokens(b.String()) > e.toolRoom() {
		// Too many to describe: the names are what get_values needs.
		b.Reset()
		for _, m := range ms {
			fmt.Fprintf(&b, "- %s: %d, latest %s\n", m.Name, m.Count, m.Latest.Observed[:min(len(m.Latest.Observed), 10)])
		}
	}
	return b.String(), nil
}

// matchNames finds the measurement names a request means: exact first,
// then any that contain it or it contains.
func (e *toolEnv) matchNames(want string) ([]string, []string, error) {
	ms, err := e.st.Measurements(e.pid)
	if err != nil {
		return nil, nil, err
	}
	var all []string
	for _, m := range ms {
		all = append(all, m.Name)
	}
	w := strings.ToLower(strings.TrimSpace(want))
	for _, n := range all {
		if strings.ToLower(n) == w {
			return []string{n}, all, nil
		}
	}
	var hits []string
	for _, n := range all {
		l := strings.ToLower(n)
		if w != "" && (strings.Contains(l, w) || strings.Contains(w, l)) {
			hits = append(hits, n)
		}
	}
	return hits, all, nil
}

// inRange compares by day, so a reading at 2026-10-01T08:00 is "on"
// 2026-10-01.
func inRange(d, from, to string) bool {
	day := d
	if len(day) > 10 {
		day = day[:10]
	}
	return (from == "" || day >= from) && (to == "" || day <= to)
}

// maxChartPoints is where a chart switches to one point a day (the
// average), so months of readings every few minutes stay readable.
const maxChartPoints = 400

func daily(ps []chartPoint) []chartPoint {
	var out []chartPoint
	var sum float64
	var n int
	var out1 bool
	flush := func(day string) {
		if n > 0 {
			out = append(out, chartPoint{Date: day, Value: sum / float64(n), Out: out1})
		}
	}
	day := ""
	for _, p := range ps {
		d := p.Date
		if len(d) > 10 {
			d = d[:10]
		}
		if d != day {
			flush(day)
			day, sum, n, out1 = d, 0, 0, false
		}
		sum += p.Value
		n++
		out1 = out1 || p.Out
	}
	flush(day)
	return out
}

func (e *toolEnv) getValues(name, from, to string) (string, error) {
	names, all, err := e.matchNames(name)
	if err != nil {
		return "", err
	}
	e.note("get_values", "Looked up: "+name, 0)
	if len(names) == 0 {
		return fmt.Sprintf("No measurement called %q. On record: %s.", name, strings.Join(all, ", ")), nil
	}
	var b strings.Builder
	for _, n := range names {
		obs, err := e.st.ObservationsIn(e.pid, n, from, to)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s, %d reading%s", n, len(obs), plural(len(obs)))
		if from != "" || to != "" {
			fmt.Fprintf(&b, " from %s to %s", or(from, "the start"), or(to, "now"))
		}
		b.WriteString(":\n")
		b.WriteString(valueStats(obs))
		rows := obs[max(len(obs)-maxValueRows, 0):]
		var lines []string
		for left, i := e.toolRoom()/(2*len(names)), len(rows)-1; i >= 0; i-- {
			line := "- " + obsLine(rows[i])
			if left -= ollama.Tokens(line); left < 0 {
				break
			}
			lines = append(lines, line)
		}
		slices.Reverse(lines)
		if len(lines) < len(obs) {
			fmt.Fprintf(&b, "The latest %d:\n", len(lines))
		}
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return b.String(), nil
}

// maxValueRows is how many readings get_values lists one by one; beyond
// that the averages carry it, so years of daily values fit the context.
const maxValueRows = 40

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// valueStats sums readings up so the model can quote an average instead
// of working one out: overall, and month by month when they span months,
// or year by year when they span more than two years.
func valueStats(obs []store.Observation) string {
	type agg struct {
		sum, lo, hi float64
		n           int
	}
	add := func(g *agg, v float64) {
		if g.n == 0 {
			g.lo, g.hi = v, v
		}
		g.sum += v
		g.n++
		g.lo, g.hi = min(g.lo, v), max(g.hi, v)
	}
	var all agg
	span := map[string]bool{}
	for _, o := range obs {
		span[o.Observed[:min(len(o.Observed), 7)]] = true
	}
	by, keyLen := "month", 7
	if len(span) > 24 {
		by, keyLen = "year", 4
	}
	months := map[string]*agg{}
	var order []string
	for _, o := range obs {
		if o.Value == nil {
			continue
		}
		k := o.Observed[:min(len(o.Observed), keyLen)]
		if months[k] == nil {
			months[k] = &agg{}
			order = append(order, k)
		}
		add(&all, *o.Value)
		add(months[k], *o.Value)
	}
	if all.n < 2 {
		return ""
	}
	r := func(f float64) string { return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) }
	s := fmt.Sprintf("Average %s, lowest %s, highest %s.\n", r(all.sum/float64(all.n)), r(all.lo), r(all.hi))
	if len(order) > 1 {
		s += "By " + by + ":\n"
		for _, k := range order {
			g := months[k]
			s += fmt.Sprintf("- %s: average %s, lowest %s, highest %s (%d)\n", k, r(g.sum/float64(g.n)), r(g.lo), r(g.hi), g.n)
		}
	}
	return s
}

func (e *toolEnv) chart(want []string, from, to, title string) (string, error) {
	spec := chartSpec{Title: title}
	var report []string
	var missing []string
	var all []string
	for _, w := range want {
		names, known, err := e.matchNames(w)
		if err != nil {
			return "", err
		}
		all = known
		if len(names) == 0 {
			missing = append(missing, w)
		}
		for _, n := range names {
			if len(spec.Series) == 4 {
				break
			}
			obs, err := e.st.ObservationsIn(e.pid, n, from, to)
			if err != nil {
				return "", err
			}
			s := chartSeries{Name: n}
			for _, o := range obs {
				if o.Value == nil || !inRange(o.Observed, from, to) {
					continue
				}
				s.Points = append(s.Points, chartPoint{Date: o.Observed, Value: *o.Value, Out: o.OutOfRange()})
				s.Unit = o.Unit
				if o.RefLow != nil {
					s.RefLow = o.RefLow
				}
				if o.RefHigh != nil {
					s.RefHigh = o.RefHigh
				}
			}
			if len(s.Points) == 0 {
				missing = append(missing, n+" (no numbers in that time)")
				continue
			}
			readings := len(s.Points)
			if readings > maxChartPoints {
				s.Points = daily(s.Points)
				s.Name += " (daily average)"
			}
			spec.Series = append(spec.Series, s)
			p := s.Points
			if readings > len(p) {
				report = append(report, fmt.Sprintf("%s has %d readings, drawn as %d daily averages", n, readings, len(p)))
			}
			lo, hi := p[0].Value, p[0].Value
			for _, x := range p {
				lo, hi = min(lo, x.Value), max(hi, x.Value)
			}
			report = append(report, fmt.Sprintf("%s: %d point%s from %s (%g %s) to %s (%g %s), lowest %g, highest %g",
				n, len(p), plural(len(p)), p[0].Date, p[0].Value, s.Unit, p[len(p)-1].Date, p[len(p)-1].Value, s.Unit, lo, hi))
		}
	}
	if len(spec.Series) == 0 {
		return fmt.Sprintf("Nothing to chart for %s. On record: %s.", strings.Join(want, ", "), strings.Join(all, ", ")), nil
	}
	if spec.Title == "" {
		var ns []string
		for _, s := range spec.Series {
			ns = append(ns, s.Name)
		}
		spec.Title = strings.Join(ns, ", ")
	}
	e.extra.Charts = append(e.extra.Charts, spec)
	e.emit(map[string]any{"chart": spec})
	e.note("chart", "Charted: "+spec.Title, 0)
	out := "The chart is shown to the user. " + strings.Join(report, "; ") + "."
	if len(missing) > 0 {
		out += " Not found: " + strings.Join(missing, ", ") + "."
	}
	return out, nil
}

// normalizeBloodType turns "o positive" or "AB neg" into the form the
// profile uses ("O+", "AB−").
func normalizeBloodType(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" {
		return ""
	}
	if strings.Contains(l, "know") || strings.Contains(l, "unsure") {
		return "Don't know"
	}
	group := ""
	for _, g := range []string{"ab", "a", "b", "o"} {
		if strings.HasPrefix(l, g) {
			group = strings.ToUpper(g)
			break
		}
	}
	sign := ""
	switch {
	case strings.Contains(l, "+") || strings.Contains(l, "pos"):
		sign = "+"
	case strings.Contains(l, "-") || strings.Contains(l, "−") || strings.Contains(l, "neg"):
		sign = "−"
	}
	if group == "" || sign == "" {
		return s
	}
	return group + sign
}

func normalizeSex(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "m", "male", "man":
		return "Male"
	case "f", "female", "woman":
		return "Female"
	case "intersex":
		return "Intersex"
	}
	return s
}

// ---------- changes ----------

func (e *toolEnv) changed(id int64, summary string) {
	c := change{ActivityID: id, Summary: summary}
	e.extra.Changes = append(e.extra.Changes, c)
	e.emit(map[string]any{"change": c})
}

func (e *toolEnv) noteValue(a map[string]any) (string, error) {
	o := store.Observation{PersonID: e.pid, Name: sArg(a, "name"), ValueText: sArg(a, "value_text"), Unit: sArg(a, "unit"),
		Observed: sArg(a, "date"), Note: sArg(a, "note"), Source: "chat"}
	switch v := a["value"].(type) {
	case float64:
		o.Value = &v
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			o.Value = &f
		} else if o.ValueText == "" {
			o.ValueText = v
		}
	}
	if o.Name == "" || (o.Value == nil && o.ValueText == "") {
		return "", errors.New("say what was measured and its value")
	}
	if o.Observed == "" {
		o.Observed = e.today.Format("2006-01-02")
	}
	if d, err := time.Parse("2006-01-02", o.Observed); err != nil || d.After(e.today.AddDate(0, 0, 1)) {
		return "", errors.New("the date must be YYYY-MM-DD and not in the future")
	}
	// Reuse the existing spelling of a known measurement.
	if names, _, err := e.matchNames(o.Name); err == nil {
		for _, n := range names {
			if strings.EqualFold(n, o.Name) {
				o.Name = n
			}
		}
	}
	v := o.ValueText
	if o.Value != nil {
		v = strconv.FormatFloat(*o.Value, 'f', -1, 64)
	}
	summary := strings.TrimSpace(fmt.Sprintf("Noted %s %s %s on %s", strings.ToLower(o.Name), v, o.Unit, o.Observed))
	var aid int64
	err := e.st.Tx(func(tx *sql.Tx) error {
		id, err := store.AddObservationTx(tx, o)
		if err != nil {
			return err
		}
		aid, err = store.LogIDTx(tx, e.pid, 0, "assistant", summary, undoOp{Op: "observation", ObservationIDs: []int64{id}})
		return err
	})
	if err != nil {
		return "", err
	}
	e.changed(aid, summary)
	return "Saved. " + summary + ". The user sees it with an Undo button.", nil
}

func (e *toolEnv) updateProfile(field, action, value, name, detail, since string) (string, error) {
	p, err := e.st.Person(e.pid)
	if err != nil {
		return "", err
	}
	prevRaw, _ := json.Marshal(p.Profile)
	prev := json.RawMessage(prevRaw)
	pr := &p.Profile
	lists := map[string]*[]store.Item{
		"conditions": &pr.Conditions, "surgeries": &pr.Surgeries, "allergies": &pr.Allergies, "medications": &pr.Medications,
		"family_history": &pr.FamilyHistory, "vaccinations": &pr.Vaccinations, "providers": &pr.Providers,
	}
	scalars := map[string]*string{
		"born": &pr.Born, "sex": &pr.Sex, "height": &pr.Height, "blood_type": &pr.BloodType, "lifestyle": &pr.Lifestyle,
		"emergency_contact": &pr.EmergencyContact, "insurance": &pr.Insurance, "notes": &pr.Notes,
	}
	label := strings.ReplaceAll(field, "_", " ")
	var summary string
	if list, ok := lists[field]; ok {
		if name == "" {
			name = value
		}
		if name == "" {
			return "", errors.New("say which item")
		}
		idx := -1
		for i, it := range *list {
			if strings.EqualFold(it.Name, name) {
				idx = i
			}
		}
		switch action {
		case "add", "set":
			it := store.Item{Name: name, Detail: detail, Since: since}
			if idx >= 0 {
				old := (*list)[idx]
				if detail == "" {
					it.Detail = old.Detail
				}
				if since == "" {
					it.Since = old.Since
				}
				(*list)[idx] = it
				summary = fmt.Sprintf("Updated %s in %s", name, label)
			} else {
				*list = append(*list, it)
				summary = fmt.Sprintf("Added %s to %s", name, label)
			}
		case "remove":
			if idx < 0 {
				return fmt.Sprintf("%s isn't in %s; nothing changed.", name, label), nil
			}
			*list = append((*list)[:idx], (*list)[idx+1:]...)
			summary = fmt.Sprintf("Removed %s from %s", name, label)
		default:
			return "", errors.New("action must be add or remove")
		}
	} else if dst, ok := scalars[field]; ok {
		if action == "remove" {
			value = ""
		}
		if field == "born" && value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return "", errors.New("born must be YYYY-MM-DD")
			}
		}
		if field == "blood_type" {
			value = normalizeBloodType(value)
		}
		if field == "sex" {
			value = normalizeSex(value)
		}
		*dst = value
		if value == "" {
			summary = "Cleared " + label
		} else {
			summary = fmt.Sprintf("Set %s to %s", label, value)
		}
	} else {
		return "", fmt.Errorf("there's no profile field %q", field)
	}
	var aid int64
	err = e.st.Tx(func(tx *sql.Tx) error {
		if err := store.UpdatePersonTx(tx, p); err != nil {
			return err
		}
		var err error
		aid, err = store.LogIDTx(tx, e.pid, 0, "assistant", summary, undoOp{Op: "profile", PersonID: e.pid, PrevName: p.Name, PrevProfile: &prev})
		return err
	})
	if err != nil {
		return "", err
	}
	e.changed(aid, summary)
	return "Saved to the profile. " + summary + ". The user sees it with an Undo button.", nil
}
