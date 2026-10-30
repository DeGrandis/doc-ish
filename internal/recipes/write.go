package recipes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/ollama"
)

// Draft is a recipe the model wrote and the app tried on the whole file.
type Draft struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Script      string   `json:"script"`
	Records     []Record `json:"-"`
	Problems    []string `json:"problems,omitempty"`
	Attempts    int      `json:"attempts"`
}

// Preview is what the review screen shows of a draft's output.
type Preview struct {
	Total    int      `json:"total"`
	Series   []Series `json:"series"`
	Sample   []Record `json:"sample"`
	Problems []string `json:"problems,omitempty"`
}

// PreviewOf summarizes records for a person to check.
func PreviewOf(recs []Record, problems []string) Preview {
	p := Preview{Total: len(recs), Series: Summarize(recs), Problems: problems}
	// The first rows and the last few, so both ends can be checked.
	if len(recs) <= 15 {
		p.Sample = recs
	} else {
		p.Sample = append(append([]Record{}, recs[:12]...), recs[len(recs)-3:]...)
	}
	return p
}

const writeInstructions = `You write import recipes for a family's private medical records app. A recipe is a short program in Starlark (a small dialect of Python: no imports, no classes, no try/except, no f-strings; use "+" or "%" to build strings) that reads one exported file, usually a table from a device or a portal, and hands back every reading.

The file is already loaded for you. There is NO open(), no import and no file access of any kind: the file's contents are in the variable text, and csv_dicts() and csv_rows() read it when called with no text argument.

What a recipe has:
- text: the whole file as a string.
- csv_rows(text=None, delimiter="", skip=0): the file (or text) as a list of rows, each a list of strings. skip drops that many lines first. The delimiter is guessed if empty.
- csv_dicts(text=None, delimiter="", skip=0): the same, as a list of dicts keyed by the header row (the first row after skip).
- number(s): s as a float, or None if it isn't a plain number. "1,234.5", "84,3" (a decimal comma) and "1.234,5" all work; "<0.5" and "" give None.
- date(s, day_first=False): "YYYY-MM-DD", or None. Understands 2026-03-04, 03/04/2026, Mar 4, 2026 and more; day_first=True reads 03/04/2026 as 3 April.
- datetime(s, day_first=False): "YYYY-MM-DDTHH:MM" when s has a time, else "YYYY-MM-DD"; or None.
- json.decode(s) for JSON.
- emit(name, value=None, unit="", date="", text="", low=None, high=None, flag="", note=""): hand back one reading. name is the plain name of what was measured ("Glucose", "Weight", "Heart rate", "Blood pressure systolic"). value is a number; use text instead for a result that isn't one. date is from date() or datetime(). low/high are the reference range when the file gives one.

Rules:
- One emit per reading. A row with several measurements (systolic, diastolic, pulse) gives one emit for each.
- Skip lines before the header (device name, serial number), blank lines, and rows without a reading. Never emit totals, averages or other summary rows.
- Every emit needs a date. Skip a row whose date can't be read.
- Use the units the file states, from the header if that's where they are.
- low and high only when the file itself has a reference range for that row. Never write in ranges from your own knowledge.
- Use these names when they fit, so readings from different files line up: "Weight", "Body fat", "Blood pressure systolic", "Blood pressure diastolic", "Heart rate", "Glucose", "Temperature", "Oxygen saturation", "Steps", "Sleep".
- Emit every measurement column, not just the first (a scale's weight and body fat are two emits).
- Keep it short and plain. No error handling beyond "if x == None: continue".

Example, for a file whose first lines are "Glucose Meter Export", "Device: GM-200", "", then the header "Date,Time,Glucose (mg/dL),Meal":
rows = csv_dicts(skip=3)
for r in rows:
    when = datetime(r["Date"] + " " + r["Time"])
    g = number(r["Glucose (mg/dL)"])
    if when == None or g == None:
        continue
    emit("Glucose", g, unit="mg/dL", date=when, note=r["Meal"])

Answer as JSON: name (a short name for this format, like "Glucose meter export"), kind (device, lab or other), description (one sentence on what the file holds), script.`

var writeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"name":        map[string]any{"type": "string"},
		"kind":        map[string]any{"type": "string", "enum": []string{"device", "lab", "other"}},
		"description": map[string]any{"type": "string"},
		"script":      map[string]any{"type": "string"},
	},
	"required": []string{"name", "kind", "description", "script"},
}

// WriteRequest is what the model is told about the file.
type WriteRequest struct {
	Model    string
	NumCtx   int
	Filename string
	Text     string // the whole file
	Feedback string // what a person said was wrong with the last draft
	Previous string // that draft's script
	Today    time.Time
}

const maxAttempts = 4

// Write asks the model for a recipe, tries it, and on an error or a bad
// result shows the model what went wrong, up to three times. The draft
// that comes back has been run on the whole file.
func Write(ctx context.Context, c *ollama.Client, req WriteRequest) (Draft, error) {
	lines := strings.Split(strings.ReplaceAll(req.Text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	sample := lines // the drafts are tried on the first 300 lines
	if len(sample) > 300 {
		sample = sample[:300]
	}
	shown := strings.Join(lines, "\n")
	if len(lines) > 45 {
		shown = strings.Join(lines[:40], "\n") + fmt.Sprintf("\n[…%d more lines…]\n", len(lines)-45) + strings.Join(lines[len(lines)-5:], "\n")
	}
	if len(shown) > 12000 {
		shown = strings.ToValidUTF8(shown[:12000], "") + "\n[…cut…]"
	}

	var user strings.Builder
	fmt.Fprintf(&user, "File name: %s\nIt has %d lines. Today is %s.\n\nThe file:\n<<<\n%s\n>>>\n", req.Filename, len(lines), req.Today.Format("2006-01-02"), shown)
	// Spell out where the table starts: the most common mistake is the
	// wrong skip.
	if _, at := header(req.Text); at >= 0 {
		rows, _ := parseTable(lines[at], Delimiter(req.Text), 0)
		if len(rows) > 0 {
			var keys []string
			for _, c := range rows[0] {
				keys = append(keys, fmt.Sprintf("%q", strings.TrimSpace(c)))
			}
			fmt.Fprintf(&user, "\nThe header row is line %d, so csv_dicts(skip=%d) gives one dict per data row with exactly these keys: %s. (csv_rows(skip=%d) gives lists instead, header first.)\n",
				at+1, at, strings.Join(keys, ", "), at)
		}
	}
	if req.Previous != "" {
		fmt.Fprintf(&user, "\nAn earlier recipe for this file:\n%s\n", req.Previous)
	}
	if req.Feedback != "" {
		fmt.Fprintf(&user, "\nWhat the person said is wrong with it: %s\nWrite a recipe that fixes that.\n", req.Feedback)
	}
	msgs := []ollama.Message{{Role: "system", Content: writeInstructions}, {Role: "user", Content: user.String()}}

	var d Draft
	var lastErr error
	for d.Attempts = 1; d.Attempts <= maxAttempts; d.Attempts++ {
		raw, err := c.ChatJSON(ctx, ollama.ChatRequest{Model: req.Model, Messages: msgs, NumCtx: req.NumCtx}, writeSchema)
		if err != nil {
			return d, err
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil || strings.TrimSpace(d.Script) == "" {
			lastErr = errors.New("the model's answer had no recipe in it")
			msgs = append(msgs, ollama.Message{Role: "assistant", Content: raw},
				ollama.Message{Role: "user", Content: "That wasn't a recipe. Answer with JSON whose script field is the Starlark program."})
			continue
		}
		problem := tryDraft(ctx, d.Script, strings.Join(sample, "\n"), req.Today)
		if problem == "" {
			// It works on the sample: now the whole file.
			rctx, cancel := context.WithTimeout(ctx, time.Minute)
			out, err := Run(rctx, d.Script, req.Text, DefaultLimits)
			cancel()
			if err == nil {
				d.Records, d.Problems = Validate(out.Records, req.Today)
				rows := DataRows(req.Text)
				switch {
				case len(d.Records) == 0:
					problem = "On the whole file it emitted nothing usable. " + strings.Join(d.Problems, "; ")
				case rows > 0 && len(d.Records) < rows*8/10 && d.Attempts < maxAttempts:
					// Rows skipped without a word are the worst failure:
					// it looks like it worked.
					problem = fmt.Sprintf("It found only %d values in %d rows of data, so most rows were skipped. Look at how the dates and numbers in the skipped rows are written, and read every row that has a reading.", len(d.Records), rows)
				default:
					if rows > 0 && len(d.Records) < rows*8/10 {
						d.Problems = append([]string{fmt.Sprintf("only %d values came from %d rows: check that nothing was missed", len(d.Records), rows)}, d.Problems...)
					}
					return d, nil
				}
			} else {
				problem = "On the whole file it failed: " + err.Error() + hint(err.Error())
			}
		}
		lastErr = errors.New(problem)
		msgs = append(msgs, ollama.Message{Role: "assistant", Content: raw},
			ollama.Message{Role: "user", Content: "I ran that recipe and it didn't work:\n" + problem +
				"\nFix exactly that and answer with the whole recipe again. Remember: Starlark has no try/except, no import and no open()."})
	}
	d.Attempts = maxAttempts
	return d, fmt.Errorf("after %d tries the recipe still didn't work: %v", maxAttempts, lastErr)
}

// hint explains the mistakes small models make most in Starlark, which
// the bare error doesn't.
func hint(err string) string {
	switch {
	case strings.Contains(err, "undefined: open"), strings.Contains(err, "undefined: file"), strings.Contains(err, "undefined: read"):
		return "\nThere is no open() and no file to read: the whole file is already in the variable text. Call csv_dicts() or csv_rows() with no text argument, for example rows = csv_dicts(skip=3)."
	case strings.Contains(err, "got try"), strings.Contains(err, "got except"), strings.Contains(err, "undefined: try"):
		return "\nStarlark has no try/except. Check values with number() and date(), and skip a row with continue when they give None."
	case strings.Contains(err, "got import"), strings.Contains(err, "undefined: import"):
		return "\nStarlark has no import. Everything you need is already there: text, csv_rows, csv_dicts, number, date, datetime, json, emit."
	case strings.Contains(err, "not in dict"), strings.Contains(err, "key "):
		return "\nA column name doesn't match. Use the header row's names exactly, and set skip so the header is the first row csv_dicts() sees (count the lines above it, blank ones too)."
	case strings.Contains(err, "got string literal"), strings.Contains(err, "f-string"):
		return "\nStarlark has no f-strings: build strings with + or %."
	case strings.Contains(err, "list index: got string"):
		return "\ncsv_rows() gives lists, read by position (r[0], r[1]). To read by column name (r[\"Date\"]), use csv_dicts() instead."
	case strings.Contains(err, "dict") && strings.Contains(err, "got int"):
		return "\ncsv_dicts() gives dicts, read by column name (r[\"Date\"]). To read by position, use csv_rows() instead."
	}
	return ""
}

// tryDraft runs a draft on the start of the file and says what's wrong.
func tryDraft(ctx context.Context, script, sample string, today time.Time) string {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := Run(rctx, script, sample, Limits{Steps: 20_000_000, Records: 20_000})
	if err != nil {
		return "It stopped with an error:\n" + err.Error() + hint(err.Error())
	}
	keep, problems := Validate(out.Records, today)
	if len(keep) == 0 {
		msg := "It ran but emitted no readings."
		if len(problems) > 0 {
			msg += " Problems: " + strings.Join(problems, "; ")
		}
		if len(out.Printed) > 0 {
			msg += " It printed: " + strings.Join(out.Printed, " / ")
		}
		return msg
	}
	if len(problems) > 0 && len(problems) >= len(keep) {
		return "Most readings were wrong: " + strings.Join(problems, "; ")
	}
	return ""
}
