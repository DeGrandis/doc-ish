// Package pdfgen writes simple text PDFs, for invented test documents:
// lines and table cells placed where they'd be on a printed page, so the
// PDF tools read them back the way they read a real report.
package pdfgen

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"strings"
)

// Text is one run of text on a page, from the bottom-left corner in points
// (a letter page is 612 by 792).
type Text struct {
	X, Y float64
	Size float64 // points; 0 means 10
	S    string
}

// Page is everything on one page.
type Page []Text

// Lines lays out lines top to bottom from y, each a row of cells at the
// given x positions (one cell for plain text).
func Lines(y, step float64, xs []float64, rows ...[]string) Page {
	var p Page
	for _, r := range rows {
		for i, cell := range r {
			if cell != "" && i < len(xs) {
				p = append(p, Text{X: xs[i], Y: y, S: cell})
			}
		}
		y -= step
	}
	return p
}

// Scanned makes a PDF of JPEG page images with no text in it, the way a
// scanner or fax does.
func Scanned(jpegs [][]byte) ([]byte, error) {
	var objs []string
	add := func(s string) int {
		objs = append(objs, s)
		return len(objs)
	}
	add("<< /Type /Catalog /Pages 2 0 R >>")
	add("")
	var kids []string
	for _, j := range jpegs {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(j))
		if err != nil {
			return nil, err
		}
		img := add(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
			cfg.Width, cfg.Height, len(j), j))
		ops := "q 612 0 0 792 0 0 cm /Im1 Do Q"
		content := add(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(ops), ops))
		page := add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /XObject << /Im1 %d 0 R >> >> >>", content, img))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	return assemble(objs), nil
}

// Write makes the PDF.
func Write(pages []Page) []byte {
	var objs []string
	add := func(s string) int {
		objs = append(objs, s)
		return len(objs)
	}
	add("<< /Type /Catalog /Pages 2 0 R >>")
	add("") // the page tree, filled in below
	font := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	var kids []string
	esc := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
	for _, p := range pages {
		var ops strings.Builder
		ops.WriteString("BT ")
		for _, t := range p {
			size := t.Size
			if size == 0 {
				size = 10
			}
			fmt.Fprintf(&ops, "/F1 %g Tf 1 0 0 1 %g %g Tm (%s) Tj ", size, t.X, t.Y, esc.Replace(t.S))
		}
		ops.WriteString("ET")
		content := add(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", ops.Len(), ops.String()))
		page := add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", content, font))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	return assemble(objs)
}

// assemble numbers the objects and writes the file around them.
func assemble(objs []string) []byte {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return []byte(b.String())
}
