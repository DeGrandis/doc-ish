// Package ingest reads a stored file into something a model can make
// sense of (text, or images for a vision model), then asks the model
// what it is and pulls out its values. It never stores anything itself.
package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Content is what extraction found in a file.
type Content struct {
	Text      string   // the readable text, if any
	Images    [][]byte // JPEG pages or photos, for a vision model
	Pages     int      // pages in a PDF
	Rows      int      // data rows in a CSV
	Sampled   bool     // only part of a big table or document was kept
	Note      string   // how it was read, for the review screen
	Dicom     *Dicom   // tags, for a DICOM image
	Extractor string   // what read it: text, pdftotext, pdf-images, image, dicom, docx
	Full      string   // a table's whole text, for an import recipe
	// A PDF's text page by page (a scanned page is empty), and the page
	// number (from 1) each image is of, so a long file can be read a few
	// pages at a time.
	PageTexts  []string
	ImagePages []int
	// Doubts about how it was read, each a reason for a person to look
	// before it's filed.
	Doubts []string
}

// ErrUnsupported means there is no way to read this type yet.
var ErrUnsupported = errors.New("can't read this type of file yet")

const (
	maxRead     = 8 << 20 // bytes of a text file considered
	maxCSVRows  = 150     // rows of a table given to the model
	maxPDFPages = 300     // scanned pages of a PDF read (by Tesseract, or a few at a time by a model)
	maxImageDim = 1600    // pixels on the longest side sent to a vision model
)

// Extract reads r (the plain bytes of a stored file). tmpDir is where a
// PDF may be written briefly for the PDF tools; use memory-backed storage
// (/dev/shm) where there is one.
func Extract(ctx context.Context, mime, filename string, r io.Reader, tmpDir string) (Content, error) {
	base := strings.SplitN(mime, ";", 2)[0]
	switch {
	case base == "application/dicom":
		return extractDicom(r)
	case base == "application/pdf":
		return extractPDF(ctx, r, tmpDir)
	case base == "image/heic" || base == "image/heif":
		return Content{}, errors.New("HEIC photos can't be read yet; on an iPhone, share or export the photo as JPEG")
	case strings.HasPrefix(base, "image/"):
		return extractImage(r)
	case base == "text/csv" || base == "text/tab-separated-values":
		return extractCSV(r)
	case strings.Contains(base, "wordprocessingml"): // before the xml case: its type ends in +xml
		return extractDocx(r)
	case strings.HasPrefix(base, "text/") || strings.Contains(base, "json") || strings.Contains(base, "xml"):
		return extractText(r)
	}
	return Content{}, ErrUnsupported
}

func readCapped(r io.Reader) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxRead+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxRead {
		return b[:maxRead], true, nil
	}
	return b, false, nil
}

func toText(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if utf8.Valid(b) {
		return string(b)
	}
	// Latin-1, as old exports often are.
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func extractText(r io.Reader) (Content, error) {
	b, cut, err := readCapped(r)
	if err != nil {
		return Content{}, err
	}
	return Content{Text: toText(b), Sampled: cut, Extractor: "text"}, nil
}

// extractCSV keeps the header and the first rows of a big table: a model
// can tell what a table is from a sample, but can't copy out thousands of
// rows. (Recipes, later, read whole tables without the model.)
func extractCSV(r io.Reader) (Content, error) {
	b, cut, err := readCapped(r)
	if err != nil {
		return Content{}, err
	}
	full := toText(b)
	lines := strings.Split(strings.ReplaceAll(full, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	c := Content{Rows: max(len(lines)-1, 0), Extractor: "text", Sampled: cut, Full: full}
	if len(lines) > maxCSVRows+1 {
		c.Sampled = true
		c.Note = fmt.Sprintf("A table of %d rows; the reader saw the first %d.", c.Rows, maxCSVRows)
		lines = lines[:maxCSVRows+1]
	}
	c.Text = strings.Join(lines, "\n")
	return c, nil
}

// extractPDF uses Poppler's pdftotext; a page with almost no text is a
// scan, so the first pages are rendered as images for a vision model.
func extractPDF(ctx context.Context, r io.Reader, tmpDir string) (Content, error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return Content{}, errors.New("the PDF tools (poppler-utils) aren't installed on the server")
	}
	dir, err := os.MkdirTemp(tmpDir, "read-*")
	if err != nil {
		return Content{}, err
	}
	defer os.RemoveAll(dir)
	pdf := filepath.Join(dir, "in.pdf")
	f, err := os.OpenFile(pdf, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return Content{}, err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return Content{}, err
	}
	f.Close()

	c := Content{Pages: pdfPages(ctx, pdf), Extractor: "pdftotext"}
	// Fixed pitch keeps a column at the same place on every page, so a
	// table running over a page break still lines up under its header
	// (plain -layout lays out each page on its own).
	out, err := exec.CommandContext(ctx, "pdftotext", "-layout", "-fixed", "4", "-enc", "UTF-8", pdf, "-").Output()
	if err != nil {
		return Content{}, fmt.Errorf("couldn't read the PDF: %w", err)
	}
	// pdftotext ends each page with a form feed.
	c.PageTexts = strings.Split(strings.TrimSuffix(string(out), "\f"), "\f")
	c.PageTexts = dedent(c.PageTexts)
	c.Pages = max(c.Pages, len(c.PageTexts))
	c.Text = strings.TrimSpace(strings.Join(c.PageTexts, "\n\n"))

	// A PDF with almost no text is a scan; so is a page with none in an
	// otherwise text PDF (a faxed page among printed ones; a cover page
	// with a few words isn't). Scans are rendered for a vision model.
	allScan := len(strings.Fields(c.Text))/max(len(c.PageTexts), 1) < 15
	var scans []int
	for i, p := range c.PageTexts {
		if allScan || len(strings.Fields(p)) < 3 {
			scans = append(scans, i+1)
		}
	}
	if len(scans) == 0 {
		return c, nil
	}
	if len(scans) == len(c.PageTexts) {
		c.Extractor = "pdf-images"
		c.Note = "A scanned PDF; the reader looked at it as images."
	} else {
		c.Note = fmt.Sprintf("%d of its %d pages are scans; the reader looked at those as images.", len(scans), c.Pages)
	}
	if len(scans) > maxPDFPages {
		c.Sampled = true
		c.Note = fmt.Sprintf("%d of its %d pages are scans; the reader looked at the first %d of them as images.", len(scans), c.Pages, maxPDFPages)
		scans = scans[:maxPDFPages]
	}
	for _, n := range scans {
		prefix := filepath.Join(dir, fmt.Sprintf("page-%d", n))
		if err := exec.CommandContext(ctx, "pdftoppm", "-r", "130", "-jpeg", "-singlefile", "-f", fmt.Sprint(n), "-l", fmt.Sprint(n), pdf, prefix).Run(); err != nil {
			return Content{}, fmt.Errorf("couldn't render page %d of the PDF: %w", n, err)
		}
		b, err := os.ReadFile(prefix + ".jpg")
		if err != nil {
			return Content{}, err
		}
		img, err := shrink(b)
		if err != nil {
			return Content{}, err
		}
		c.Images = append(c.Images, img)
		c.ImagePages = append(c.ImagePages, n)
	}
	return c, nil
}

// dedent takes off the margin every line shares, and trailing space.
func dedent(pages []string) []string {
	margin := -1
	for _, p := range pages {
		for _, l := range strings.Split(p, "\n") {
			if t := strings.TrimLeft(l, " "); t != "" {
				if n := len(l) - len(t); margin < 0 || n < margin {
					margin = n
				}
			}
		}
	}
	out := make([]string, len(pages))
	for i, p := range pages {
		lines := strings.Split(p, "\n")
		for k, l := range lines {
			l = strings.TrimRight(l, " ")
			if len(l) >= margin && margin > 0 {
				l = l[margin:]
			}
			lines[k] = l
		}
		out[i] = strings.Trim(strings.Join(lines, "\n"), "\n")
	}
	return out
}

func pdfPages(ctx context.Context, pdf string) int {
	out, err := exec.CommandContext(ctx, "pdfinfo", pdf).Output()
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "Pages:"); ok {
			var n int
			fmt.Sscan(strings.TrimSpace(v), &n)
			return n
		}
	}
	return 0
}

func extractImage(r io.Reader) (Content, error) {
	b, _, err := readCapped(r)
	if err != nil {
		return Content{}, err
	}
	img, err := shrink(b)
	if err != nil {
		return Content{}, err
	}
	return Content{Images: [][]byte{img}, Extractor: "image"}, nil
}

// shrink re-encodes an image as JPEG no larger than maxImageDim, which is
// plenty for a model to read print and keeps requests small.
func shrink(b []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("couldn't open the image: %w", err)
	}
	bnd := src.Bounds()
	w, h := bnd.Dx(), bnd.Dy()
	if s := max(w, h); s > maxImageDim {
		w, h = w*maxImageDim/s, h*maxImageDim/s
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src) // flatten transparency onto white
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bnd, xdraw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// extractDocx pulls the paragraphs out of a Word document.
func extractDocx(r io.Reader) (Content, error) {
	b, _, err := readCapped(r)
	if err != nil {
		return Content{}, err
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return Content{}, errors.New("this Word file looks damaged")
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Content{}, err
		}
		defer rc.Close()
		var sb strings.Builder
		dec := xml.NewDecoder(io.LimitReader(rc, maxRead))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.CharData:
				sb.Write(t)
			case xml.EndElement:
				if t.Name.Local == "p" || t.Name.Local == "tr" {
					sb.WriteByte('\n')
				}
				if t.Name.Local == "tc" {
					sb.WriteByte('\t')
				}
			}
		}
		return Content{Text: strings.TrimSpace(sb.String()), Extractor: "docx"}, nil
	}
	return Content{}, errors.New("this Word file has no document in it")
}
