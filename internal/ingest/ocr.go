package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/DeGrandis/doc-ish/internal/ollama"
)

// An OCR model (DeepSeek-OCR, made for reading documents) can turn scanned
// pages and photos of paperwork into text before the reader sees them.
// The reader then treats them like printed pages: read a few at a time,
// tables and all, with a text model, instead of asking a general vision
// model to read the print and pick out the values in one go. A picture
// with little text in it (an X-ray, a rash) stays a picture for the vision
// model.

// ocrPrompts are tried in order. In trials on invented records, the plain
// Markdown prompt read text, notes and tables best; now and then a table
// comes back as one run of text with its cells glued together, and the
// layout prompt reads that page better.
var ocrPrompts = []string{"Convert the document to markdown.", "<|grounding|>Given the layout of the image."}

const (
	ocrTokens  = 4096
	ocrCtx     = 8192
	ocrTimeout = 3 * time.Minute
	ocrWords   = 15 // fewer than this and it isn't a document page
)

// Progress hears how far a long job has got: what it's doing, and how
// many of how many steps are done.
type Progress func(what string, done, total int)

var (
	groundTag = regexp.MustCompile(`(?s)<\|ref\|>.*?<\|/ref\|><\|det\|>.*?<\|/det\|>\n?`)
	stray     = regexp.MustCompile(`<\|[a-z_/]+\|>`)
	tableTag  = regexp.MustCompile(`(?is)<table.*?</table>`)
	rowTag    = regexp.MustCompile(`(?is)<tr.*?</tr>`)
	cellTag   = regexp.MustCompile(`(?is)<t[dh][^>]*>(.*?)</t[dh]>`)
	anyTag    = regexp.MustCompile(`(?s)<[^>]+>`)
)

// cleanOCR takes the layout labels out of the model's answer and turns
// HTML tables into rows of cells separated by " | ", with empty cells
// kept, so columns still line up with their header.
func cleanOCR(s string) string {
	s = groundTag.ReplaceAllString(s, "")
	s = stray.ReplaceAllString(s, "")
	s = tableTag.ReplaceAllStringFunc(s, func(t string) string {
		var rows []string
		for _, tr := range rowTag.FindAllString(t, -1) {
			var cells []string
			for _, m := range cellTag.FindAllStringSubmatch(tr, -1) {
				cells = append(cells, strings.TrimSpace(html.UnescapeString(anyTag.ReplaceAllString(m[1], " "))))
			}
			rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
		}
		return "\n" + strings.Join(rows, "\n") + "\n"
	})
	return strings.TrimSpace(s)
}

// collapsed reports an HTML table with no rows: its cells glued into one
// string, so which number is in which column is lost.
func collapsed(s string) bool {
	for _, t := range tableTag.FindAllString(s, -1) {
		if !strings.Contains(strings.ToLower(t), "<tr") {
			return true
		}
	}
	return false
}

var errCollapsed = errors.New("the OCR model ran a table's cells together")

func ocrImage(ctx context.Context, c ollama.Chatter, model string, img []byte) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(img)
	for _, prompt := range ocrPrompts {
		msg := ollama.Message{Role: "user", Content: prompt, Images: []string{b64}}
		text, _, err := c.Chat(ctx, ollama.ChatRequest{Model: model, Messages: []ollama.Message{msg}, NumCtx: ocrCtx, NumPredict: ocrTokens, Exact: true},
			func(string) {})
		if err != nil {
			return "", err
		}
		if !collapsed(text) {
			return cleanOCR(text), nil
		}
	}
	return "", errCollapsed
}

// OCR reads a file's images with the OCR model, in place: a page that
// turns out to be a document becomes text (in PageTexts for a PDF, or as
// the file's text for a photo) and leaves Images; anything else stays.
// When the OCR model fails, the image stays for the vision model.
func OCR(ctx context.Context, c ollama.Chatter, model string, cont *Content, progress Progress) error {
	if model == "" || len(cont.Images) == 0 || cont.Dicom != nil {
		return nil
	}
	var keep [][]byte
	var keepPages []int
	var photoText []string
	read, failed := 0, 0
	for i, img := range cont.Images {
		page := 0
		if i < len(cont.ImagePages) {
			page = cont.ImagePages[i]
		}
		if progress != nil {
			what := "reading the photo's text"
			if page > 0 {
				what = fmt.Sprintf("reading scanned page %d", page)
			}
			progress(what, i, len(cont.Images))
		}
		octx, cancel := context.WithTimeout(ctx, ocrTimeout)
		text, err := ocrImage(octx, c, model, img)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || len(strings.Fields(text)) < ocrWords {
			if err != nil {
				failed++
			}
			keep = append(keep, img)
			if page > 0 {
				keepPages = append(keepPages, page)
			}
			continue
		}
		read++
		if page > 0 && page <= len(cont.PageTexts) {
			cont.PageTexts[page-1] = text
		} else {
			photoText = append(photoText, text)
		}
	}
	cont.Images, cont.ImagePages = keep, keepPages
	if len(cont.PageTexts) > 0 {
		cont.Text = strings.TrimSpace(strings.Join(cont.PageTexts, "\n\n"))
	} else if len(photoText) > 0 {
		cont.Text = strings.TrimSpace(cont.Text + "\n\n" + strings.Join(photoText, "\n\n"))
	}
	if read > 0 {
		cont.Extractor += "+ocr"
		note := fmt.Sprintf("The OCR model read %d scanned page%s into text.", read, plural(read))
		if len(cont.PageTexts) == 0 {
			note = "The OCR model read the photo into text."
		}
		if len(keep) > 0 || cont.Sampled {
			note += " " + cont.Note // what it said about the pages still images, or the pages left out
		}
		cont.Note = strings.TrimSpace(note)
	}
	short := shortRows(cont.PageTexts)
	if len(cont.PageTexts) == 0 && len(shortRows(photoText)) > 0 {
		short = []int{0}
	}
	switch {
	case len(short) == 1 && short[0] == 0:
		cont.Doubts = append(cont.Doubts, "A row of a table in the photo has fewer cells than the table's header, so some values may be under the wrong column.")
	case len(short) > 0:
		cont.Doubts = append(cont.Doubts, fmt.Sprintf("On %s, a table row has fewer cells than the table's header, so some values may be under the wrong column (a date, say).", pageList(short)))
	}
	if failed > 0 {
		cont.Note = strings.TrimSpace(cont.Note + fmt.Sprintf(" The OCR model couldn't read %d page%s; the vision model looked at them instead.", failed, plural(failed)))
	}
	return nil
}

// pageList is "page 7" or "pages 5, 6 and 7".
func pageList(pages []int) string {
	if len(pages) == 1 {
		return fmt.Sprintf("page %d", pages[0])
	}
	var ns []string
	for _, p := range pages {
		ns = append(ns, fmt.Sprint(p))
	}
	return "pages " + strings.Join(ns[:len(ns)-1], ", ") + " and " + ns[len(ns)-1]
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
