package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/ollama"
)

// A long file (a 100-page records release, a binder of scanned faxes) is
// more than a model can take in at once, and more than it can answer in
// one go. It's read a few pages at a time:
//
//   - Lines printed at the top or bottom of most pages (the patient's
//     name, "Page 12 of 117", the print date) are taken off every page and
//     shown once, so they don't crowd out the content.
//   - Each part is told what carries over from the pages before it: the
//     last section heading, the last date line, and, when a table runs
//     over a page break, the table's header, which is only printed above
//     its first rows.
//   - Scanned pages go to the vision model a couple at a time.
//   - The parts' values are put together with their own dates (a records
//     release covers years), the same value read twice is kept once, and
//     a last short call describes the whole file from the parts.

const (
	partChars     = 8000 // characters of text in one part
	pagesPerPart  = 3    // and pages: dense tables make long answers
	imagesPerPart = 2
	maxQuestions  = 12
	partTimeout   = 5 * time.Minute
)

// isLong reports whether a file should be read in parts.
func isLong(c Content) bool {
	return len(c.PageTexts) > 1 && (len(c.Text) > partChars*3/2 || len(c.Images) > 3)
}

type part struct {
	first, last int // pages, from 1
	text        string
	images      [][]byte
	carried     string // what carries over from the pages before
}

func (p part) pages() string {
	if p.first == p.last {
		return fmt.Sprintf("Page %d", p.first)
	}
	return fmt.Sprintf("Pages %d–%d", p.first, p.last)
}

// ---------- headers and footers ----------

var (
	digitRun = regexp.MustCompile(`\d+`)
	spaceRun = regexp.MustCompile(`\s+`)
	cellGap  = regexp.MustCompile(`\s{2,}`)
)

func normLine(l string) string {
	return spaceRun.ReplaceAllString(digitRun.ReplaceAllString(strings.ToLower(strings.TrimSpace(l)), "#"), " ")
}

// edgeLines are the indexes of a page's first and last few non-empty lines.
func edgeLines(lines []string) []int {
	const zone = 4
	var idx []int
	for i := 0; i < len(lines) && len(idx) < zone; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			idx = append(idx, i)
		}
	}
	n := 0
	for i := len(lines) - 1; i >= 0 && n < zone; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			n++
			if !contains(idx, i) {
				idx = append(idx, i)
			}
		}
	}
	return idx
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// stripRepeated takes off lines printed at the top or bottom of most
// pages, and returns one copy of each. A table header repeated on every
// page stays: it's content.
func stripRepeated(pages []string) ([]string, []string) {
	if len(pages) < 3 {
		return pages, nil
	}
	// A scan's header reads a little differently on each page ("JORDAN",
	// "] ORDAN"), so lines that are nearly the same count as one.
	var keys []string
	canon := func(n string) string {
		for _, k := range keys {
			if nearly(k, n) {
				return k
			}
		}
		keys = append(keys, n)
		return n
	}
	count := map[string]int{}
	for _, p := range pages {
		lines := strings.Split(p, "\n")
		seen := map[string]bool{}
		for _, i := range edgeLines(lines) {
			n := canon(normLine(lines[i]))
			if n != "" && !seen[n] && !isHeader(lines[i]) {
				seen[n] = true
				count[n]++
			}
		}
	}
	need := max(3, (len(pages)+1)/2)
	out := make([]string, len(pages))
	var once []string
	have := map[string]bool{}
	for pi, p := range pages {
		lines := strings.Split(p, "\n")
		drop := map[int]bool{}
		for _, i := range edgeLines(lines) {
			n := canon(normLine(lines[i]))
			if count[n] >= need && !isHeader(lines[i]) {
				drop[i] = true
				if !have[n] {
					have[n] = true
					once = append(once, strings.TrimSpace(lines[i]))
				}
			}
		}
		var keep []string
		for i, l := range lines {
			if !drop[i] {
				keep = append(keep, l)
			}
		}
		out[pi] = strings.Trim(strings.Join(keep, "\n"), "\n")
	}
	return out, once
}

// nearly reports two lines the same but for a few letters: one in 15
// may differ, the way OCR misreads a letter or a space here and there.
func nearly(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	allow := max(len(ra), len(rb)) / 15
	if allow == 0 || abs(len(ra)-len(rb)) > allow {
		return false
	}
	// Levenshtein distance, a row at a time, giving up once every cell
	// of a row is over.
	prev, cur := make([]int, len(rb)+1), make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > allow {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)] <= allow
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ---------- what carries over ----------

var (
	headerWords = regexp.MustCompile(`(?i)\b(test|tests|result|results|value|units?|range|reference|ref|flag|component|analyte|date|collected|normal|name|measure|description|medication|dose|directions|status|started|vaccine|given|frequency)\b`)
	dateCell    = regexp.MustCompile(`^(\d{1,2}/\d{1,2}/\d{2,4}|\d{4}-\d{2}-\d{2})$`)
	anyDate     = regexp.MustCompile(`\b(\d{1,2}/\d{1,2}/\d{2,4}|\d{4}-\d{2}-\d{2}|(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? \d{1,2},? \d{4})\b`)
	dateWords   = regexp.MustCompile(`(?i)\b(collected|reported|received|date|visit|encounter|service|admit|admitted|discharge|discharged|signed|seen|performed|drawn)\b`)
	numberCell  = regexp.MustCompile(`^[<>≤≥]?=?\s*-?\d+([.,]\d+)?\s*[A-Za-z%*]{0,3}$`)
)

// cells splits a line of a table: columns set apart by two or more spaces,
// or a row of a Markdown table from the OCR model ("| a | b |").
func cells(line string) []string {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 3 {
		var out []string
		for _, c := range strings.Split(strings.Trim(t, "|"), "|") {
			if c = strings.TrimSpace(c); c != "" && strings.Trim(c, "-: ") != "" {
				out = append(out, c)
			}
		}
		return out
	}
	var out []string
	for _, c := range cellGap.Split(t, -1) {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// isHeader is a table's header line: several cells naming columns, or a
// cumulative report's dates, and no results.
func isHeader(line string) bool {
	cs := cells(line)
	if len(cs) < 3 {
		return false
	}
	words, dates := 0, 0
	for _, c := range cs {
		switch {
		case dateCell.MatchString(c):
			dates++
		case numberCell.MatchString(c):
			return false
		case headerWords.MatchString(c):
			words++
		}
	}
	return words >= 2 || dates >= 2
}

// isRow is a line of a table: a name and at least one number.
func isRow(line string) bool {
	cs := cells(line)
	if len(cs) < 2 || numberCell.MatchString(cs[0]) {
		return false
	}
	for _, c := range cs[1:] {
		if numberCell.MatchString(c) {
			return true
		}
	}
	return false
}

// isHeading is a short title line: "LIPID PANEL", "Progress Note", "Assessment:".
func isHeading(line string) bool {
	l := strings.TrimSpace(line)
	if len(l) < 3 || len(l) > 60 || len(cells(l)) != 1 || strings.ContainsAny(l, "0123456789") || strings.HasSuffix(l, ".") || strings.HasSuffix(l, ",") {
		return false
	}
	if len(strings.Fields(l)) > 6 {
		return false
	}
	return strings.HasSuffix(l, ":") || l == strings.ToUpper(l) && strings.ToLower(l) != l
}

type carry struct {
	header, date, heading string
}

// follow reads a page's lines and returns what carries over after it, and
// whether the page opens in the middle of a table.
func follow(page string, in carry) (carry, bool) {
	out := in
	decided, opensMid := false, false
	for _, l := range strings.Split(page, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		switch {
		case isHeader(l):
			out.header = strings.TrimSpace(l)
			decided = true
		case isRow(l):
			// Rows before any header on the page (a subheading may come
			// first) belong to the table carried over.
			if !decided {
				opensMid, decided = in.header != "", true
			}
		case isHeading(l):
			out.heading = strings.TrimSpace(l)
		case len(strings.Fields(l)) >= 12 && len(cells(l)) == 1:
			// Running text: whatever table there was has ended.
			out.header, decided = "", true
		}
		if anyDate.MatchString(l) && dateWords.MatchString(l) && !isRow(l) {
			out.date = clip(strings.TrimSpace(spaceRun.ReplaceAllString(l, " ")), 140)
		}
	}
	return out, opensMid
}

func (c carry) describe(opensMid bool) string {
	var b strings.Builder
	if c.heading != "" {
		fmt.Fprintf(&b, "- The last section heading: %s\n", c.heading)
	}
	if c.date != "" {
		fmt.Fprintf(&b, "- The last date line: %s\n", c.date)
	}
	if opensMid {
		fmt.Fprintf(&b, "- The table at the top of these pages continues from the page before. Its header, printed only above its first rows, was:\n%s\n", c.header)
	}
	return b.String()
}

// ---------- planning the parts ----------

func planParts(c Content, pages []string) []part {
	images := map[int][]byte{}
	for i, n := range c.ImagePages {
		if i < len(c.Images) {
			images[n] = c.Images[i]
		}
	}
	var parts []part
	var cur *part
	flush := func() {
		if cur != nil && (strings.TrimSpace(cur.text) != "" || len(cur.images) > 0) {
			parts = append(parts, *cur)
		}
		cur = nil
	}
	var state carry
	for i, text := range pages {
		n := i + 1
		if img, ok := images[n]; ok {
			if cur != nil && (cur.images == nil || len(cur.images) >= imagesPerPart) {
				flush()
			}
			if cur == nil {
				cur = &part{first: n}
			}
			cur.last = n
			cur.images = append(cur.images, img)
			if t := strings.TrimSpace(text); t != "" {
				cur.text += fmt.Sprintf("[page %d, its few lines of text]\n%s\n", n, t)
			}
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		// A page too big for one part is split by lines.
		chunks := []string{text}
		if len(text) > partChars {
			chunks = splitLines(text, partChars)
		}
		for k, chunk := range chunks {
			label := fmt.Sprintf("[page %d]", n)
			if k > 0 {
				label = fmt.Sprintf("[page %d, continued]", n)
			}
			if cur != nil && (cur.images != nil || len(cur.text)+len(chunk) > partChars || k == 0 && n-cur.first >= pagesPerPart) {
				flush()
			}
			next, opensMid := follow(chunk, state)
			if cur == nil {
				cur = &part{first: n, carried: state.describe(opensMid)}
			}
			cur.last = n
			cur.text += label + "\n" + chunk + "\n"
			state = next
		}
	}
	flush()
	return parts
}

func splitLines(text string, size int) []string {
	var out []string
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		if b.Len()+len(l) > size && b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteString(l + "\n")
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// ---------- reading ----------

const partInstructions = `

This is one part of a long file, read a few pages at a time. Describe only these pages. Give every observation its date (YYYY-MM-DD), the day it was collected or measured, from these pages or the context given; a long file covers many dates. Never copy anything from the context: it's only there so you know where these pages are. If these pages have no results, leave observations empty.`

var errNoVision = errors.New("no model for images is picked in Settings")

func readPart(ctx context.Context, c ollama.Chatter, req Request, p part, total int, repeated []string) (Result, error) {
	model := req.ChatModel
	if len(p.images) > 0 {
		if model = req.VisionModel; model == "" {
			return Result{}, errNoVision
		}
	}
	var u strings.Builder
	fmt.Fprintf(&u, "File name: %s\nWhose records: %s\nToday: %s\n", req.Filename, req.PersonName, req.Today.Format("2006-01-02"))
	fmt.Fprintf(&u, "These are %s of a %d-page file.\n", strings.ToLower(p.pages()), total)
	if len(repeated) > 0 {
		fmt.Fprintf(&u, "\nPrinted at the top or bottom of every page (taken off the pages below):\n%s\n", strings.Join(repeated, "\n"))
	}
	if p.carried != "" {
		fmt.Fprintf(&u, "\nContext from the pages before (don't copy values from it):\n%s", p.carried)
	}
	if t := strings.TrimSpace(p.text); t != "" {
		fmt.Fprintf(&u, "\nThe text of these pages:\n<<<\n%s\n>>>\n", t)
	}
	msg := ollama.Message{Role: "user", Content: u.String()}
	if len(p.images) > 0 {
		msg.Content += fmt.Sprintf("\nThe pages are shown in the %d attached image(s), in order.\n", len(p.images))
		for _, img := range p.images {
			msg.Images = append(msg.Images, base64.StdEncoding.EncodeToString(img))
		}
	}
	raw, err := c.ChatJSON(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: []ollama.Message{{Role: "system", Content: instructions + partInstructions}, msg},
		NumCtx:   req.NumCtx,
	}, schema)
	if err != nil {
		return Result{}, err
	}
	return Parse(raw)
}

// halve splits a part that couldn't be read into two smaller ones.
func halve(p part) (part, part, bool) {
	if len(p.images) > 1 {
		a, b := p, p
		a.images, b.images = p.images[:1], p.images[1:]
		a.last, b.first = p.first, p.first+1
		return a, b, true
	}
	if len(p.images) > 0 || len(p.text) < 2000 {
		return p, p, false
	}
	lines := strings.Split(p.text, "\n")
	mid := len(lines) / 2
	a, b := p, p
	a.text, b.text = strings.Join(lines[:mid], "\n"), strings.Join(lines[mid:], "\n")
	next, opensMid := follow(a.text, carry{})
	b.carried = next.describe(opensMid)
	return a, b, true
}

type partResult struct {
	part
	res Result
	err error
}

// readLong reads a file in parts and puts them together.
func readLong(ctx context.Context, c ollama.Chatter, req Request) (Result, error) {
	pages, repeated := stripRepeated(req.Content.PageTexts)
	pages = tabulate(pages)
	queue := planParts(req.Content, pages)
	total := max(req.Content.Pages, len(pages))
	var done []partResult
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if req.Progress != nil {
			req.Progress(fmt.Sprintf("reading %s of %d", strings.ToLower(p.pages()), total), len(done), len(done)+len(queue)+2)
		}
		pctx, cancel := context.WithTimeout(ctx, partTimeout)
		res, err := readPart(pctx, c, req, p, total, repeated)
		timedOut := pctx.Err() != nil && ctx.Err() == nil
		cancel()
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return Result{}, ctx.Err()
		case errors.Is(err, errUnreadable) || timedOut:
			// Too much for one answer: try it in halves.
			if a, b, ok := halve(p); ok {
				queue = append([]part{a, b}, queue...)
				continue
			}
			if timedOut {
				err = errors.New("the model took too long on it")
			}
		case errors.Is(err, errNoVision):
		default:
			return Result{}, err // Ollama is down or the model is missing: try the whole file later
		}
		done = append(done, partResult{p, res, err})
	}
	sort.SliceStable(done, func(i, j int) bool { return done[i].first < done[j].first })
	out := merge(req, done)
	if req.Progress != nil {
		req.Progress("summing up the whole file", len(done), len(done)+1)
	}
	describeWhole(ctx, c, req, done, total, &out)
	return out, nil
}

// merge puts the parts' answers together.
func merge(req Request, parts []partResult) Result {
	var out Result
	seen := map[string]bool{}
	meds := map[string]bool{}
	names := map[string]int{}
	var questions []string
	low, unsure, read := 0, 0, 0
	for _, p := range parts {
		if p.err != nil {
			questions = append(questions, fmt.Sprintf("%s couldn't be read: %v.", p.pages(), p.err))
			continue
		}
		read++
		r := p.res
		switch r.Confidence {
		case "low":
			low++
		case "medium":
			unsure++
		}
		if r.PatientName != "" {
			names[r.PatientName]++
			if req.PersonName != "" && !NameMatches(req.PersonName, r.PatientName) {
				out.PatientName = r.PatientName // a page that may be someone else's is what Review should see
			}
		}
		for _, v := range r.Observations {
			if v.Date == "" {
				v.Date = r.DocDate
			}
			k := strings.ToLower(fmt.Sprintf("%s|%v|%s|%s|%s", v.Name, num(v.Value), v.ValueText, v.Unit, v.Date))
			if seen[k] {
				continue
			}
			seen[k] = true
			out.Observations = append(out.Observations, v)
		}
		for _, m := range r.Medications {
			if k := strings.ToLower(m.Name); !meds[k] {
				meds[k] = true
				out.Medications = append(out.Medications, m)
			}
		}
		for _, f := range r.FollowUps {
			// Each note's follow-ups count from that note's date.
			if f.From == "" {
				f.From = r.DocDate
			}
			k := strings.ToLower(f.What + "|" + f.From)
			if f.What == "" || seen["follow:"+k] {
				continue
			}
			seen["follow:"+k] = true
			out.FollowUps = append(out.FollowUps, f)
		}
		for _, q := range r.Questions {
			questions = append(questions, p.pages()+": "+q)
		}
	}
	if out.PatientName == "" {
		best := 0
		for n, k := range names {
			if k > best {
				out.PatientName, best = n, k
			}
		}
	}
	switch {
	case read == 0 || low*2 > read:
		out.Confidence = "low"
	case low > 0 || unsure > 0 || read < len(parts):
		out.Confidence = "medium"
	default:
		out.Confidence = "high"
	}
	if len(questions) > maxQuestions {
		questions = append(questions[:maxQuestions-1], fmt.Sprintf("…and %d more notes like these.", len(questions)-maxQuestions+1))
	}
	out.Questions = questions
	return out
}

func num(f *float64) string {
	if f == nil {
		return ""
	}
	return fmt.Sprint(*f)
}

var wholeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"kind":     map[string]any{"type": "string", "enum": Kinds},
		"title":    map[string]any{"type": "string"},
		"doc_date": map[string]any{"type": "string"},
		"provider": map[string]any{"type": "string"},
		"summary":  map[string]any{"type": "string"},
	},
	"required": []string{"kind", "title", "doc_date", "provider", "summary"},
}

// describeWhole names and summarizes the file from its parts, falling back
// to the parts' own answers.
func describeWhole(ctx context.Context, c ollama.Chatter, req Request, parts []partResult, total int, out *Result) {
	kinds, providers := map[string]int{}, map[string]int{}
	var lines []string
	for _, p := range parts {
		if p.err != nil {
			continue
		}
		r := p.res
		kinds[r.Kind]++
		if r.Provider != "" {
			providers[r.Provider]++
		}

		if out.Title == "" {
			out.Title = r.Title
		}
		lines = append(lines, fmt.Sprintf("- %s: %s, “%s”, %s, %s. %s (%d values)", p.pages(), r.Kind, r.Title, or(r.DocDate, "no date"),
			or(r.Provider, "no provider"), r.Summary, len(r.Observations)))
	}
	out.Kind, out.Provider = most(kinds, "other"), most(providers, "")
	// The file's date is its newest result's (a model asked for it tends
	// to give the print date, or today's).
	newest := ""
	for _, o := range out.Observations {
		if o.Date > newest && o.Date <= req.Today.Format("2006-01-02") {
			newest = o.Date
		}
	}
	out.DocDate = newest
	var parts3 []string
	for _, p := range parts {
		if p.err == nil && len(parts3) < 3 && p.res.Summary != "" {
			parts3 = append(parts3, p.res.Summary)
		}
	}
	out.Summary = strings.Join(parts3, " ")
	if len(lines) > 0 {
		model := req.ChatModel
		if model == "" {
			model = req.VisionModel
		}
		u := fmt.Sprintf("File name: %s\nWhose records: %s\nToday: %s\n\nA %d-page file was read in parts. What each part held:\n%s\n\n"+
			"Describe the whole file: kind (what most of it is), title (a short name a person would recognize, like \"Health records release, 2025–2026\"), "+
			"doc_date (YYYY-MM-DD: the most recent visit or result in it), provider, and summary (three or four plain sentences: what it holds and the period it covers).",
			req.Filename, req.PersonName, req.Today.Format("2006-01-02"), total, clip(strings.Join(lines, "\n"), 20000))
		wctx, cancel := context.WithTimeout(ctx, partTimeout)
		raw, err := c.ChatJSON(wctx, ollama.ChatRequest{Model: model, NumCtx: req.NumCtx,
			Messages: []ollama.Message{{Role: "system", Content: "You describe medical files for a family's private records app, as JSON."}, {Role: "user", Content: u}}}, wholeSchema)
		cancel()
		if err == nil {
			if w, err := Parse(raw); err == nil {
				if w.Title != "" {
					out.Title = w.Title
				}
				if w.Kind != "other" || out.Kind == "other" {
					out.Kind = w.Kind
				}
				if out.DocDate == "" {
					out.DocDate = w.DocDate
				}
				if w.Provider != "" {
					out.Provider = w.Provider
				}
				if w.Summary != "" {
					out.Summary = w.Summary
				}
			}
		}
	}
	out.Summary = strings.TrimSpace(out.Summary + fmt.Sprintf(" (Read in %d parts, %d pages.)", len(parts), total))
}

func most(m map[string]int, def string) string {
	best, out := 0, def
	for k, n := range m {
		if n > best || n == best && k < out {
			best, out = n, k
		}
	}
	return out
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
