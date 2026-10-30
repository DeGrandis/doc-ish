package ingest

import (
	"regexp"
	"strings"
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
