package recipes

import (
	"encoding/csv"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

// The helpers a recipe can call, besides Starlark's own string methods
// and the json module.

// Delimiter guesses a table's separator from its first lines.
func Delimiter(text string) rune {
	lines := strings.SplitN(text, "\n", 40)
	best, bestScore := ',', -1
	for _, d := range []rune{',', '\t', ';', '|'} {
		counts := map[int]int{}
		for _, l := range lines {
			if n := strings.Count(l, string(d)); n > 0 {
				counts[n]++
			}
		}
		for n, c := range counts {
			if score := c * 10; score > bestScore && n > 0 {
				best, bestScore = d, score
			}
		}
	}
	return best
}

// parseTable reads text as delimited rows, forgiving ragged lines.
func parseTable(text string, delim rune, skip int) ([][]string, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if skip > 0 {
		if skip >= len(lines) {
			return nil, nil
		}
		lines = lines[skip:]
	}
	r := csv.NewReader(strings.NewReader(strings.Join(lines, "\n")))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func tableArgs(fn string, args starlark.Tuple, kwargs []starlark.Tuple, thread *starlark.Thread) ([][]string, error) {
	var src starlark.Value = starlark.None
	delim := ""
	skip := 0
	if err := starlark.UnpackArgs(fn, args, kwargs, "text?", &src, "delimiter?", &delim, "skip?", &skip); err != nil {
		return nil, err
	}
	text := ""
	switch s := src.(type) {
	case starlark.String:
		text = string(s)
	case starlark.NoneType:
		if g, ok := thread.Local("text").(string); ok {
			text = g
		}
	default:
		return nil, fmt.Errorf("%s: text must be a string", fn)
	}
	d := Delimiter(text)
	if delim != "" {
		d = []rune(delim)[0]
	}
	return parseTable(text, d, skip)
}

// csv_rows(text=None, delimiter="", skip=0) -> list of lists of strings.
// With no text it reads the whole file.
func csvRows(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	rows, err := tableArgs(b.Name(), args, kwargs, thread)
	if err != nil {
		return nil, err
	}
	out := make([]starlark.Value, len(rows))
	for i, r := range rows {
		cells := make([]starlark.Value, len(r))
		for j, c := range r {
			cells[j] = starlark.String(strings.TrimSpace(c))
		}
		out[i] = starlark.NewList(cells)
	}
	return starlark.NewList(out), nil
}

// csv_dicts(text=None, delimiter="", skip=0) -> list of dicts keyed by the
// header row (the first row after skip).
func csvDicts(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	rows, err := tableArgs(b.Name(), args, kwargs, thread)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return starlark.NewList(nil), nil
	}
	head := rows[0]
	out := make([]starlark.Value, 0, len(rows)-1)
	for _, r := range rows[1:] {
		d := starlark.NewDict(len(head))
		for j, h := range head {
			v := ""
			if j < len(r) {
				v = strings.TrimSpace(r[j])
			}
			d.SetKey(starlark.String(strings.TrimSpace(h)), starlark.String(v))
		}
		out = append(out, d)
	}
	return starlark.NewList(out), nil
}

var (
	numberRE   = regexp.MustCompile(`^[-+]?(\d{1,3}(,\d{3})+|\d+)?(\.\d+)?$`) // 1,234.5
	europeanRE = regexp.MustCompile(`^[-+]?(\d{1,3}(\.\d{3})+|\d+)?(,\d+)?$`) // 1.234,5
	commaRE    = regexp.MustCompile(`^[-+]?\d+,(\d{1,2}|\d{4,})$`)            // 84,3: a decimal comma, not thousands
)

// parseNumber reads "1,234.5", "84,3", "1.234,5", " 98 ", "-3"; anything
// else is nil ("<0.5" is a result to keep as text, not a number). A comma
// followed by exactly three digits ("1,234") is read as thousands.
func parseNumber(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "." || s == "," || s == "-" || s == "+" {
		return nil
	}
	var plain string
	switch {
	case commaRE.MatchString(s):
		plain = strings.Replace(s, ",", ".", 1)
	case numberRE.MatchString(s):
		plain = strings.ReplaceAll(s, ",", "")
	case europeanRE.MatchString(s) && strings.Contains(s, ","):
		plain = strings.Replace(strings.ReplaceAll(s, ".", ""), ",", ".", 1)
	default:
		return nil
	}
	f, err := strconv.ParseFloat(plain, 64)
	if err != nil {
		return nil
	}
	return &f
}

// number(s) -> float, or None when s isn't a plain number.
func numberFn(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s starlark.Value
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &s); err != nil {
		return nil, err
	}
	switch x := s.(type) {
	case starlark.Int, starlark.Float:
		return x, nil
	case starlark.String:
		if f := parseNumber(string(x)); f != nil {
			return starlark.Float(*f), nil
		}
	}
	return starlark.None, nil
}

var dateLayouts = []string{
	"2006-01-02", "2006/01/02", "20060102", "Jan 2, 2006", "January 2, 2006", "2 Jan 2006", "02-Jan-2006",
}
var monthFirst = []string{"1/2/2006", "01/02/2006", "1/2/06", "1-2-2006", "01-02-2006"}
var dayFirst = []string{"2/1/2006", "02/01/2006", "2.1.2006", "02.01.2006", "2/1/06", "2-1-2006"}
var timeLayouts = []string{"T15:04:05", "T15:04", " 15:04:05", " 15:04", " 3:04 PM", " 3:04:05 PM", " 3:04PM", "T15:04:05Z07:00", "T15:04:05.000Z07:00", "T15:04:05Z"}

// parseWhen reads a date, with or without a time; dayFirst reads 3/4 as
// the 3rd of April.
func parseWhen(s string, dayFirstOrder bool) (time.Time, bool, bool) {
	s = strings.TrimSpace(s)
	dates := append([]string{}, dateLayouts...)
	if dayFirstOrder {
		dates = append(dates, dayFirst...)
	} else {
		dates = append(dates, monthFirst...)
	}
	for _, d := range dates {
		if t, err := time.Parse(d, s); err == nil {
			return t, false, true
		}
		for _, tl := range timeLayouts {
			if t, err := time.Parse(d+tl, s); err == nil {
				return t, true, true
			}
		}
	}
	return time.Time{}, false, false
}

func whenArgs(b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (string, bool, error) {
	var s string
	dayFirstOrder := false
	err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s, "day_first?", &dayFirstOrder)
	return s, dayFirstOrder, err
}

// date(s, day_first=False) -> "YYYY-MM-DD", or None when s isn't a date.
func dateFn(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	s, df, err := whenArgs(b, args, kwargs)
	if err != nil {
		return nil, err
	}
	if t, _, ok := parseWhen(s, df); ok {
		return starlark.String(t.Format("2006-01-02")), nil
	}
	return starlark.None, nil
}

// datetime(s, day_first=False) -> "YYYY-MM-DDTHH:MM" (or just the date
// when s has no time), or None.
func datetimeFn(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	s, df, err := whenArgs(b, args, kwargs)
	if err != nil {
		return nil, err
	}
	if t, hasTime, ok := parseWhen(s, df); ok {
		if hasTime {
			return starlark.String(t.Format("2006-01-02T15:04")), nil
		}
		return starlark.String(t.Format("2006-01-02")), nil
	}
	return starlark.None, nil
}
