package recipes

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var whenRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2})?)?$`)

// Validate keeps the records that make sense and describes the first few
// that don't, so a draft recipe can be fixed.
func Validate(recs []Record, today time.Time) ([]Record, []string) {
	var keep []Record
	var problems []string
	bad := 0
	limit := today.AddDate(0, 0, 1).Format("2006-01-02")
	for i, r := range recs {
		var why string
		switch {
		case r.Name == "":
			why = "no name"
		case r.Value == nil && r.Text == "":
			why = "no value"
		case !whenRE.MatchString(r.Date):
			why = fmt.Sprintf("date %q isn't YYYY-MM-DD (use date() or datetime())", r.Date)
		case r.Date[:10] > limit:
			why = fmt.Sprintf("date %s is in the future", r.Date)
		}
		if why == "" {
			if _, err := time.Parse("2006-01-02", r.Date[:10]); err != nil {
				why = fmt.Sprintf("date %q isn't a real day", r.Date)
			}
		}
		if why != "" {
			bad++
			if len(problems) < 5 {
				problems = append(problems, fmt.Sprintf("value %d (%s): %s", i+1, r.Name, why))
			}
			continue
		}
		keep = append(keep, r)
	}
	if bad > len(problems) {
		problems = append(problems, fmt.Sprintf("…and %d more", bad-len(problems)))
	}
	return keep, problems
}

// Series is one measurement in a recipe's output, for the review screen.
type Series struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Count int    `json:"count"`
	First string `json:"first"`
	Last  string `json:"last"`
}

// Summarize groups records by name.
func Summarize(recs []Record) []Series {
	by := map[string]*Series{}
	var order []string
	for _, r := range recs {
		s := by[r.Name]
		if s == nil {
			s = &Series{Name: r.Name, Unit: r.Unit, First: r.Date, Last: r.Date}
			by[r.Name] = s
			order = append(order, r.Name)
		}
		s.Count++
		if r.Date < s.First {
			s.First = r.Date
		}
		if r.Date > s.Last {
			s.Last = r.Date
		}
	}
	out := make([]Series, 0, len(order))
	for _, n := range order {
		out = append(out, *by[n])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Fingerprint names a table's format by its header row: the delimiter and
// the column names, lowercased. Two exports from the same device share a
// fingerprint even though their readings differ. Lines before the header
// (a device name, a serial number) are skipped: the header is the first
// line with as many fields as most of the table's lines.
func Fingerprint(text string) string {
	fp, _ := header(text)
	return fp
}

// DataRows counts the lines after a table's header that have content.
func DataRows(text string) int {
	_, at := header(text)
	if at < 0 {
		return 0
	}
	n := 0
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")[at+1:] {
		if strings.TrimSpace(strings.ReplaceAll(l, string(Delimiter(text)), "")) != "" {
			n++
		}
	}
	return n
}

// header finds a table's header line: its fingerprint and line number.
func header(text string) (string, int) {
	d := Delimiter(text)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[:60]
	}
	counts := map[int]int{}
	fieldsOf := make([]int, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		rows, err := parseTable(l, d, 0)
		if err != nil || len(rows) == 0 {
			continue
		}
		fieldsOf[i] = len(rows[0])
		counts[len(rows[0])]++
	}
	mode, best := 0, 0
	for n, c := range counts {
		if n > 1 && (c > best || (c == best && n > mode)) {
			mode, best = n, c
		}
	}
	if mode == 0 {
		return "", -1
	}
	for i, l := range lines {
		if fieldsOf[i] != mode {
			continue
		}
		rows, _ := parseTable(l, d, 0)
		var cols []string
		for _, c := range rows[0] {
			c = strings.ToLower(strings.Join(strings.Fields(c), " "))
			// A header names columns; a row of numbers or dates isn't one.
			if parseNumber(c) != nil {
				return "", -1
			}
			cols = append(cols, c)
		}
		return fmt.Sprintf("table %q: %s", string(d), strings.Join(cols, " | ")), i
	}
	return "", -1
}
