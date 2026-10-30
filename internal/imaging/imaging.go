// Package imaging reads DICOM images: what an image says about itself (its
// study, series, patient and dates), and its pixels, decompressed and
// scaled to the values the scanner measured, for the viewer and for a
// vision model.
//
// Uncompressed images and baseline JPEG are read in Go. Other compression
// (JPEG Lossless, JPEG-LS, JPEG 2000, RLE), common on hospital CDs, is
// undone first by GDCM's gdcmconv, which the Docker image includes.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Info is what one DICOM file says about itself.
type Info struct {
	PatientName string  `json:"patient_name,omitempty"`
	PatientBorn string  `json:"patient_born,omitempty"`
	StudyUID    string  `json:"study_uid"`
	SeriesUID   string  `json:"series_uid"`
	SOPUID      string  `json:"sop_uid"`
	StudyDate   string  `json:"study_date,omitempty"`
	Modality    string  `json:"modality,omitempty"`
	BodyPart    string  `json:"body_part,omitempty"`
	Study       string  `json:"study,omitempty"`
	Series      string  `json:"series,omitempty"`
	SeriesNum   int     `json:"series_number"`
	InstanceNum int     `json:"instance_number"`
	Institution string  `json:"institution,omitempty"`
	Rows        int     `json:"rows"`
	Cols        int     `json:"cols"`
	Frames      int     `json:"frames"`
	Position    float64 `json:"position"` // where the slice is along the body, for ordering
	Syntax      string  `json:"transfer_syntax"`
}

// ErrNotDICOM means the bytes aren't a DICOM file.
var ErrNotDICOM = errors.New("not a DICOM file")

// IsDICOM reports whether b starts like a DICOM file (the "DICM" marker
// after the 128-byte preamble).
func IsDICOM(b []byte) bool {
	return len(b) >= 132 && string(b[128:132]) == "DICM"
}

func parse(b []byte, pixels bool) (dicom.Dataset, error) {
	opts := []dicom.ParseOption{dicom.AllowMissingMetaElementGroupLength()}
	if !pixels {
		opts = append(opts, dicom.SkipPixelData())
	}
	ds, err := dicom.ParseUntilEOF(bytes.NewReader(b), nil, opts...)
	if err != nil {
		return ds, fmt.Errorf("%w: %v", ErrNotDICOM, err)
	}
	return ds, nil
}

func str(ds dicom.Dataset, t tag.Tag) string {
	el, err := ds.FindElementByTag(t)
	if err != nil {
		return ""
	}
	switch v := el.Value.GetValue().(type) {
	case []string:
		return strings.TrimSpace(strings.Join(v, "\\"))
	case []int:
		if len(v) > 0 {
			return strconv.Itoa(v[0])
		}
	}
	return ""
}

func num(ds dicom.Dataset, t tag.Tag) (float64, bool) {
	el, err := ds.FindElementByTag(t)
	if err != nil {
		return 0, false
	}
	switch v := el.Value.GetValue().(type) {
	case []int:
		if len(v) > 0 {
			return float64(v[0]), true
		}
	case []float64:
		if len(v) > 0 {
			return v[0], true
		}
	case []string:
		if len(v) > 0 {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(v[0], "\\")[0]), 64)
			return f, err == nil
		}
	}
	return 0, false
}

func numOr(ds dicom.Dataset, t tag.Tag, def float64) float64 {
	if f, ok := num(ds, t); ok {
		return f
	}
	return def
}

func dicomDate(s string) string {
	t, err := time.Parse("20060102", strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// Header reads a DICOM file's tags, without its pixels.
func Header(b []byte) (Info, error) {
	ds, err := parse(b, false)
	if err != nil {
		return Info{}, err
	}
	in := Info{
		PatientName: strings.Join(strings.FieldsFunc(str(ds, tag.PatientName), func(r rune) bool { return r == '^' }), " "),
		PatientBorn: dicomDate(str(ds, tag.PatientBirthDate)),
		StudyUID:    str(ds, tag.StudyInstanceUID),
		SeriesUID:   str(ds, tag.SeriesInstanceUID),
		SOPUID:      str(ds, tag.SOPInstanceUID),
		StudyDate:   dicomDate(str(ds, tag.StudyDate)),
		Modality:    str(ds, tag.Modality),
		BodyPart:    str(ds, tag.BodyPartExamined),
		Study:       str(ds, tag.StudyDescription),
		Series:      str(ds, tag.SeriesDescription),
		SeriesNum:   int(numOr(ds, tag.SeriesNumber, 0)),
		InstanceNum: int(numOr(ds, tag.InstanceNumber, 0)),
		Institution: str(ds, tag.InstitutionName),
		Rows:        int(numOr(ds, tag.Rows, 0)),
		Cols:        int(numOr(ds, tag.Columns, 0)),
		Frames:      int(numOr(ds, tag.NumberOfFrames, 1)),
		Syntax:      str(ds, tag.TransferSyntaxUID),
	}
	if in.Frames < 1 {
		in.Frames = 1
	}
	if loc, ok := num(ds, tag.SliceLocation); ok {
		in.Position = loc
	} else if el, err := ds.FindElementByTag(tag.ImagePositionPatient); err == nil {
		if v, ok := el.Value.GetValue().([]string); ok && len(v) >= 3 {
			in.Position, _ = strconv.ParseFloat(strings.TrimSpace(v[2]), 64)
		}
	}
	if in.StudyUID == "" || in.SOPUID == "" {
		return in, fmt.Errorf("%w: it has no study or image id", ErrNotDICOM)
	}
	if in.SeriesUID == "" {
		in.SeriesUID = in.StudyUID + ".0"
	}
	return in, nil
}

// Modalities names the common imaging types.
var Modalities = map[string]string{
	"CR": "X-ray", "DX": "X-ray", "MG": "Mammogram", "CT": "CT scan", "MR": "MRI", "US": "Ultrasound",
	"NM": "Nuclear medicine scan", "PT": "PET scan", "XA": "Angiogram", "RF": "Fluoroscopy", "OT": "Image",
	"SC": "Image", "OP": "Eye photo", "OPT": "Eye scan (OCT)", "ES": "Endoscopy", "IO": "Dental X-ray", "PX": "Dental X-ray",
}

// Title describes a study in a few words: "MRI, knee (MRI KNEE LT)".
func (in Info) Title() string {
	what := Modalities[strings.ToUpper(in.Modality)]
	if what == "" {
		what = "Medical image"
	}
	t := what
	if in.BodyPart != "" {
		t += ", " + strings.ToLower(in.BodyPart)
	}
	if in.Study != "" && !strings.EqualFold(in.Study, in.BodyPart) {
		t += " (" + in.Study + ")"
	}
	return t
}

// ---------- pixels ----------

// Pixels is one frame, as the values the scanner measured (after the
// rescale slope and intercept), or as color for color images.
type Pixels struct {
	Rows, Cols int
	Values     []float32 // grayscale, row by row; nil for color
	RGB        []byte    // color, 3 bytes a pixel; nil for grayscale
	Center     float64   // the window the file suggests (or one from the values)
	Width      float64
	Invert     bool // MONOCHROME1: higher values are darker
	Min, Max   float32
}

// Syntaxes Go reads without help.
var native = map[string]bool{
	"1.2.840.10008.1.2":      true, // implicit VR little endian
	"1.2.840.10008.1.2.1":    true, // explicit VR little endian
	"1.2.840.10008.1.2.2":    true, // explicit VR big endian
	"1.2.840.10008.1.2.1.99": true, // deflated
	"1.2.840.10008.1.2.4.50": true, // JPEG baseline
	"":                       true,
}

// ErrCompressed means an image needs gdcmconv, which isn't installed.
var ErrCompressed = errors.New("this image is compressed in a way that needs GDCM (gdcmconv), which isn't installed on the server")

// Decode reads one frame's pixels. tmpDir is where gdcmconv may work on a
// copy (memory-backed /dev/shm where there is one).
func Decode(b []byte, frameIdx int, tmpDir string) (*Pixels, error) {
	hdr, err := parse(b, false)
	if err != nil {
		return nil, err
	}
	if syntax := str(hdr, tag.TransferSyntaxUID); !native[syntax] {
		if b, err = decompress(b, tmpDir); err != nil {
			return nil, err
		}
	}
	ds, err := parse(b, true)
	if err != nil {
		return nil, err
	}
	el, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		return nil, errors.New("this DICOM file has no image in it")
	}
	info, ok := el.Value.GetValue().(dicom.PixelDataInfo)
	if !ok || len(info.Frames) == 0 {
		return nil, errors.New("this DICOM file's image couldn't be read")
	}
	if frameIdx < 0 || frameIdx >= len(info.Frames) {
		frameIdx = 0
	}
	f := info.Frames[frameIdx]
	slope := numOr(ds, tag.RescaleSlope, 1)
	if slope == 0 {
		slope = 1
	}
	intercept := numOr(ds, tag.RescaleIntercept, 0)
	photometric := strings.ToUpper(str(ds, tag.PhotometricInterpretation))
	p := &Pixels{Invert: photometric == "MONOCHROME1"}

	if f.Encapsulated {
		img, err := f.EncapsulatedData.GetImage()
		if err != nil {
			return nil, fmt.Errorf("couldn't decode the JPEG image: %w", err)
		}
		fromImage(p, img)
	} else {
		nf, err := f.GetNativeFrame()
		if err != nil {
			return nil, err
		}
		p.Rows, p.Cols = nf.Rows(), nf.Cols()
		spp := nf.SamplesPerPixel()
		n := p.Rows * p.Cols
		if spp == 3 {
			p.RGB = make([]byte, n*3)
			for i := 0; i < n*3; i++ {
				p.RGB[i] = byte(sample(nf, i))
			}
		} else {
			p.Values = make([]float32, n)
			for i := 0; i < n; i++ {
				p.Values[i] = float32(float64(sample(nf, i*spp))*slope + intercept)
			}
		}
	}
	if p.Values != nil {
		p.Min, p.Max = minMax(p.Values)
		if c, ok := num(ds, tag.WindowCenter); ok {
			if w, ok := num(ds, tag.WindowWidth); ok && w > 1 {
				p.Center, p.Width = c, w
			}
		}
		if p.Width == 0 {
			p.Center, p.Width = autoWindow(p.Values, p.Min, p.Max)
		}
	}
	return p, nil
}

// sample reads raw sample i of a native frame, whatever its integer type.
func sample(nf frame.INativeFrame, i int) int64 {
	switch d := nf.RawDataSlice().(type) {
	case []uint8:
		return int64(d[i])
	case []uint16:
		return int64(d[i])
	case []int16:
		return int64(d[i])
	case []uint32:
		return int64(d[i])
	case []int32:
		return int64(d[i])
	case []int:
		return int64(d[i])
	}
	return 0
}

// fromImage takes a decoded JPEG frame: grayscale as values, anything
// else as color.
func fromImage(p *Pixels, img image.Image) {
	b := img.Bounds()
	p.Rows, p.Cols = b.Dy(), b.Dx()
	if g, ok := img.(*image.Gray); ok {
		p.Values = make([]float32, p.Rows*p.Cols)
		for y := 0; y < p.Rows; y++ {
			for x := 0; x < p.Cols; x++ {
				p.Values[y*p.Cols+x] = float32(g.GrayAt(b.Min.X+x, b.Min.Y+y).Y)
			}
		}
		return
	}
	p.RGB = make([]byte, p.Rows*p.Cols*3)
	for y := 0; y < p.Rows; y++ {
		for x := 0; x < p.Cols; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := (y*p.Cols + x) * 3
			p.RGB[i], p.RGB[i+1], p.RGB[i+2] = byte(r>>8), byte(g>>8), byte(bl>>8)
		}
	}
}

func minMax(v []float32) (float32, float32) {
	lo, hi := float32(math.MaxFloat32), float32(-math.MaxFloat32)
	for _, x := range v {
		lo, hi = min(lo, x), max(hi, x)
	}
	return lo, hi
}

// autoWindow picks a window covering the middle 98% of values, which
// shows most images reasonably when the file suggests none.
func autoWindow(v []float32, lo, hi float32) (float64, float64) {
	if hi <= lo {
		return float64(lo), 1
	}
	const bins = 1024
	var hist [bins]int
	span := float64(hi - lo)
	for _, x := range v {
		hist[int(float64(x-lo)/span*(bins-1))]++
	}
	total, cut := len(v), len(v)/100
	a, b, acc := 0, bins-1, 0
	for i := 0; i < bins; i++ {
		if acc += hist[i]; acc > cut {
			a = i
			break
		}
	}
	acc = 0
	for i := bins - 1; i >= 0; i-- {
		if acc += hist[i]; acc > cut {
			b = i
			break
		}
	}
	_ = total
	l := float64(lo) + span*float64(a)/(bins-1)
	h := float64(lo) + span*float64(b)/(bins-1)
	if h <= l {
		h = l + 1
	}
	return (l + h) / 2, h - l
}

// decompress rewrites a compressed DICOM file as an uncompressed one with
// gdcmconv.
func decompress(b []byte, tmpDir string) ([]byte, error) {
	bin, err := exec.LookPath("gdcmconv")
	if err != nil {
		return nil, ErrCompressed
	}
	dir, err := os.MkdirTemp(tmpDir, "dicom-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
	if err := os.WriteFile(in, b, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "--raw", in, out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("couldn't decompress the image: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(out)
}

// ReadAll reads a stored file into memory, refusing anything absurd.
func ReadAll(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<30+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<30 {
		return nil, errors.New("this image file is over 1 GB")
	}
	return b, nil
}
