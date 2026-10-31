package ingest

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"math"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
)

// Printed scans (a faxed page, a scanned records release) are read with
// Tesseract first, without a model. It isn't the best reader of messy
// print, but it says where on the page every word is, so a page can be
// written out the way pdftotext writes a text PDF: each column at the
// same place on every page, blank cells left blank. An OCR model writing
// a table out as Markdown drops the blank cells of a table that runs onto
// a page without its header, and the values slide into the wrong date's
// column; laid out by position, they stay under their dates. A page
// Tesseract isn't sure of (a phone photo, handwriting, an X-ray) stays an
// image, for the OCR model or the vision model.

const (
	tessWidth   = 2550 // a letter page at 300 dpi, what Tesseract reads best: smaller scans are enlarged to it
	tessConf    = 80   // the mean word confidence a page needs to be taken as read
	tessTimeout = time.Minute
)

var tess struct {
	once sync.Once
	path string
}

// tesseractPath is "" when Tesseract isn't installed.
func tesseractPath() string {
	tess.once.Do(func() { tess.path, _ = exec.LookPath("tesseract") })
	return tess.path
}

// flagged is a result with its high or low flag read as one word.
var flagged = regexp.MustCompile(`^([<>]?\d+(?:[.,]\d+)?)([HL]{1,2})$`)

type word struct {
	text                     string
	line                     int // Tesseract's line, numbered down the page
	left, top, width, height int
	conf                     float64
}

// tessWords reads one image with Tesseract, enlarged to tessWidth. It
// returns the words and their mean confidence (0 to 100).
func tessWords(ctx context.Context, img []byte) ([]word, float64, error) {
	src, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, 0, err
	}
	b := src.Bounds()
	scale := min(max(float64(tessWidth)/float64(min(b.Dx(), b.Dy())), 1), 3) // by the short side, as if a page's width
	dst := image.NewGray(image.Rect(0, 0, int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	var in bytes.Buffer
	if err := png.Encode(&in, dst); err != nil {
		return nil, 0, err
	}
	// --psm 6: one block of text, so a table's row stays one line.
	cmd := exec.CommandContext(ctx, tesseractPath(), "stdin", "stdout", "--psm", "6", "tsv")
	// One thread: it's no slower alone, and its threads spin against
	// anything else on the machine, enough to run a page past tessTimeout.
	cmd.Env = append(cmd.Environ(), "OMP_THREAD_LIMIT=1")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("tesseract: %w", err)
	}
	var words []word
	lines := map[string]int{}
	var sum float64
	for _, l := range strings.Split(string(out), "\n")[1:] {
		f := strings.Split(l, "\t")
		if len(f) < 12 || f[0] != "5" || strings.TrimSpace(f[11]) == "" {
			continue
		}
		n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
		key := f[2] + "." + f[3] + "." + f[4] // block, paragraph, line
		if _, ok := lines[key]; !ok {
			lines[key] = len(lines)
		}
		conf, _ := strconv.ParseFloat(f[10], 64)
		text := strings.TrimSpace(f[11])
		if m := flagged.FindStringSubmatch(text); m != nil { // "11.8L": a gap too narrow to see
			text = m[1] + " " + m[2]
		}
		words = append(words, word{text: text, line: lines[key],
			left: n(6), top: n(7), width: n(8), height: n(9), conf: conf})
		sum += conf
	}
	if len(words) == 0 {
		return nil, 0, nil
	}
	return words, sum / float64(len(words)), nil
}

// layout writes pages of words out as text at a fixed pitch: a word starts
// at the column its left edge is at, the pitch being one document's
// typical letter width, so columns line up across its pages.
func layout(pages [][]word) []string {
	var widths, heights []float64
	for _, p := range pages {
		for _, w := range p {
			if n := utf8.RuneCountInString(w.text); n >= 3 && w.conf >= 60 {
				widths = append(widths, float64(w.width)/float64(n))
				heights = append(heights, float64(w.height))
			}
		}
	}
	out := make([]string, len(pages))
	if len(widths) == 0 {
		return out
	}
	sort.Float64s(widths)
	sort.Float64s(heights)
	pitch, lineHeight := widths[len(widths)/2], heights[len(heights)/2]
	for i, p := range pages {
		var rows [][]word
		for _, w := range p {
			for len(rows) <= w.line {
				rows = append(rows, nil)
			}
			rows[w.line] = append(rows[w.line], w)
		}
		var b strings.Builder
		bottom := -1
		for _, r := range rows {
			if len(r) == 0 {
				continue
			}
			sort.Slice(r, func(a, c int) bool { return r[a].left < r[c].left })
			top := r[0].top
			for _, w := range r {
				top = min(top, w.top)
			}
			if bottom >= 0 && float64(top-bottom) > lineHeight { // a gap between paragraphs
				b.WriteString("\n")
			}
			col, right := 0, 0
			for k, w := range r {
				at := int(math.Round(float64(w.left) / pitch))
				if k > 0 {
					at = max(at, col+1)
					if float64(w.left-right) < 2*pitch { // the next word of a sentence, not the next column
						at = col + 1
					}
				}
				right = w.left + w.width
				b.WriteString(strings.Repeat(" ", max(at-col, 0)))
				b.WriteString(w.text)
				col = max(at, col) + utf8.RuneCountInString(w.text)
				bottom = max(bottom, w.top+w.height)
			}
			b.WriteString("\n")
		}
		out[i] = b.String()
	}
	return dedent(out)
}

// Tesseract reads a file's images with Tesseract, in place, like OCR: a
// page it reads with confidence becomes text (in PageTexts for a PDF, or
// as the file's text for a photo) and leaves Images. Without Tesseract
// installed it does nothing.
func Tesseract(ctx context.Context, cont *Content, progress Progress) error {
	if tesseractPath() == "" || len(cont.Images) == 0 || cont.Dicom != nil {
		return nil
	}
	type page struct {
		i     int // in Images
		words []word
	}
	var read []page
	for i, img := range cont.Images {
		if progress != nil {
			what := "reading the photo's text"
			if i < len(cont.ImagePages) {
				what = fmt.Sprintf("reading scanned page %d", cont.ImagePages[i])
			}
			progress(what, i, len(cont.Images))
		}
		tctx, cancel := context.WithTimeout(ctx, tessTimeout)
		words, conf, err := tessWords(tctx, img)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && len(words) >= ocrWords && conf >= tessConf {
			read = append(read, page{i, words})
		}
	}
	if len(read) == 0 {
		return nil
	}
	pages := make([][]word, len(read))
	for k, p := range read {
		pages[k] = p.words
	}
	texts := layout(pages)
	done := map[int]bool{}
	var photoText []string
	for k, p := range read {
		done[p.i] = true
		if p.i < len(cont.ImagePages) && cont.ImagePages[p.i] <= len(cont.PageTexts) {
			cont.PageTexts[cont.ImagePages[p.i]-1] = texts[k]
		} else {
			photoText = append(photoText, texts[k])
		}
	}
	var keep [][]byte
	var keepPages []int
	for i, img := range cont.Images {
		if done[i] {
			continue
		}
		keep = append(keep, img)
		if i < len(cont.ImagePages) {
			keepPages = append(keepPages, cont.ImagePages[i])
		}
	}
	cont.Images, cont.ImagePages = keep, keepPages
	if len(cont.PageTexts) > 0 {
		cont.Text = strings.TrimSpace(strings.Join(cont.PageTexts, "\n\n"))
	} else {
		cont.Text = strings.TrimSpace(cont.Text + "\n\n" + strings.Join(photoText, "\n\n"))
	}
	cont.Extractor += "+tesseract"
	note := fmt.Sprintf("Tesseract read %d scanned page%s into text.", len(read), plural(len(read)))
	if len(cont.PageTexts) == 0 {
		note = "Tesseract read the photo's print into text."
	}
	if len(keep) > 0 || cont.Sampled {
		note += " " + cont.Note
	}
	cont.Note = strings.TrimSpace(note)
	return nil
}
