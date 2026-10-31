package ingest

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Tables in a printed PDF come out of pdftotext as columns of spaces. A
// small model reading a grid like
//
//	Test         03/10/2025   09/12/2025   03/12/2026   Units
//	Ferritin                  18 L         41           ng/mL
//
// tends to copy the first number and lose which date the others belong
// to, all the more so when a cell is blank. Because the text is laid out
// at a fixed pitch, a column is at the same place on every page, so each
// cell can be matched to its header by position and the table rewritten
// with every cell spelled out, blanks included:
//
//	| Test | 03/10/2025 | 09/12/2025 | 03/12/2026 | Units |
//	| Ferritin |  | 18 L | 41 | ng/mL |
//
// (In trials the same model went from 25 to 76 of 80 values with their
// right dates on tables written out like this.) The header carries over
// page breaks; running text ends the table.

var cellSpan = regexp.MustCompile(`\S+(?: \S+)*`) // cells: runs of text with no gap of two spaces

type span struct {
	start, end int // in characters
	text       string
}

func spansOf(line string) []span {
	var out []span
	for _, m := range cellSpan.FindAllStringIndex(line, -1) {
		out = append(out, span{utf8.RuneCountInString(line[:m[0]]), utf8.RuneCountInString(line[:m[1]]), line[m[0]:m[1]]})
	}
	return out
}

// column picks the header column a cell overlaps most, each column
// reaching to where the next one starts.
func column(cols []span, c span) int {
	best, most := 0, -1
	for i, h := range cols {
		lo, hi := h.start, 1<<30
		if i+1 < len(cols) {
			hi = cols[i+1].start
		}
		if i == 0 {
			lo = 0
		}
		ov := min(hi, c.end) - max(lo, c.start)
		if ov > most {
			best, most = i, ov
		}
	}
	return best
}

func pipeRow(cells []string) string {
	return "| " + strings.Join(cells, " | ") + " |"
}

// tabulate rewrites the column tables in a file's pages as rows of cells,
// and prints a table's header again on each page it runs onto, so every
// page reads on its own. Tables the OCR model already wrote as rows of
// cells get their header repeated the same way.
func tabulate(pages []string) []string {
	var cols []span   // the current table's columns, by position (printed text)
	var header string // and its header as a row of cells (printed or OCR)
	out := make([]string, len(pages))
	for pi, p := range pages {
		lines := strings.Split(p, "\n")
		carried := header != "" // a table running on from the page before
		repeat := func(i int) {
			if carried {
				lines[i] = "(the table continues from the page before; its header:)\n" + header + "\n" + lines[i]
				carried = false
			}
		}
		for i, l := range lines {
			t := strings.TrimSpace(l)
			switch {
			case t == "":
			case strings.HasPrefix(t, "|"): // already a row of cells
				if isHeader(l) {
					cols, header, carried = nil, t, false
				} else if isRow(l) {
					repeat(i)
				}
			case isHeader(l):
				cols = spansOf(l)
				var names []string
				for _, c := range cols {
					names = append(names, c.text)
				}
				header, carried = pipeRow(names), false
				lines[i] = header
			case cols != nil && isRow(l):
				row := make([]string, len(cols))
				for _, c := range spansOf(l) {
					k := column(cols, c)
					row[k] = strings.TrimSpace(row[k] + " " + c.text)
				}
				lines[i] = pipeRow(row)
				repeat(i)
			case len(strings.Fields(l)) >= 12 && len(cells(l)) == 1:
				cols, header, carried = nil, "", false // running text: the table has ended
			}
		}
		out[pi] = strings.Join(lines, "\n")
	}
	return out
}

func pipeCells(row string) int {
	return len(strings.Split(strings.Trim(strings.TrimSpace(row), "|"), "|"))
}

// shortRows lists the pages (from 1) where a row the OCR model wrote as
// cells has fewer of them than its table's header: it left out a blank
// cell, so the values after it may sit under the wrong column.
func shortRows(pages []string) []int {
	var out []int
	want := 0
	for pi, p := range pages {
		short := false
		for _, l := range strings.Split(p, "\n") {
			t := strings.TrimSpace(l)
			switch {
			case !strings.HasPrefix(t, "|"):
				if len(strings.Fields(t)) >= 12 {
					want = 0 // running text ends the table
				}
			case isHeader(l):
				want = pipeCells(t)
			case want > 0 && isRow(l) && pipeCells(t) < want:
				short = true
			}
		}
		if short {
			out = append(out, pi+1)
		}
	}
	return out
}

var cellNumber = regexp.MustCompile(`^[<>]?(\d+(?:\.\d+)?)`)

// cellDate reads a date in a table's header as YYYY-MM-DD.
func cellDate(s string) string {
	for _, layout := range []string{"1/2/2006", "1/2/06", "2006-01-02", "1-2-2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// tableValues checks what a model read from a table with a date in each
// column against the table itself. Even with every cell spelled out a
// model can get a date wrong (it has read 03/12/2026 as 2025-03-12) or
// skip a cell of a row it read, and a result lost or under the wrong date
// is worse than none. So a value takes the date of the column it's in,
// and a row the model read gets the cells it left out. A value that isn't
// in such a table, or that's in two columns, keeps the date it has, and a
// row the model didn't read is left alone: it may not be a result at all.
func tableValues(text string, vals []Value) []Value {
	type cell struct {
		value      float64
		date, flag string
	}
	type row struct {
		name  string
		cells []cell
	}
	var rows []row
	var header []string // each column's date, "" for the others
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		dates, n := make([]string, len(cells)), 0
		for i, c := range cells {
			if dates[i] = cellDate(c); dates[i] != "" {
				n++
			}
		}
		if n >= 2 {
			header = dates
			continue
		}
		if header == nil {
			continue
		}
		r := row{name: strings.ToLower(strings.TrimSpace(cells[0]))}
		for i, c := range cells {
			if i >= len(header) || header[i] == "" {
				continue
			}
			c = strings.TrimSpace(c)
			m := cellNumber.FindStringSubmatch(c)
			if m == nil {
				continue
			}
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				r.cells = append(r.cells, cell{v, header[i], strings.TrimSpace(c[len(m[0]):])})
			}
		}
		if r.name != "" && len(r.cells) > 0 {
			rows = append(rows, r)
		}
	}
	// A row is the one with the value's name exactly, or failing that in
	// part ("A1c" for "Hemoglobin A1c").
	rowsOf := func(name string) []row {
		name = strings.ToLower(strings.TrimSpace(name))
		var exact, part []row
		for _, r := range rows {
			if r.name == name {
				exact = append(exact, r)
			} else if name != "" && (strings.Contains(r.name, name) || strings.Contains(name, r.name)) {
				part = append(part, r)
			}
		}
		if len(exact) > 0 {
			return exact
		}
		return part
	}
	type key struct {
		name string
		date string
	}
	read := map[key]bool{}
	for i := range vals {
		v := &vals[i]
		if v.Value != nil {
			var dates []string
			for _, r := range rowsOf(v.Name) {
				for _, c := range r.cells {
					if c.value == *v.Value {
						dates = append(dates, c.date)
					}
				}
			}
			if len(dates) > 0 && !slices.Contains(dates, v.Date) && !slices.ContainsFunc(dates, func(d string) bool { return d != dates[0] }) {
				v.Date = dates[0]
			}
		}
		for _, r := range rowsOf(v.Name) {
			read[key{r.name, v.Date}] = true
		}
	}
	for _, v := range slices.Clone(vals) {
		for _, r := range rowsOf(v.Name) {
			for _, c := range r.cells {
				if k := (key{r.name, c.date}); !read[k] {
					read[k] = true
					value := c.value
					vals = append(vals, Value{Name: v.Name, Value: &value, Unit: v.Unit, RefLow: v.RefLow, RefHigh: v.RefHigh, Flag: c.flag, Date: c.date})
				}
			}
		}
	}
	return vals
}
