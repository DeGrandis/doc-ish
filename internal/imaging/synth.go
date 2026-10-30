package imaging

import (
	"bytes"
	"fmt"
	"math"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Synth makes an invented CT-like slice, for tests and for trying the
// viewer without anyone's real images: a round "body" of soft tissue with
// a brighter ring and a dark spot that moves from slice to slice.
type Synth struct {
	Patient   string
	StudyUID  string
	SeriesUID string
	Series    string
	SeriesNum int
	Date      string // YYYYMMDD
	Modality  string
	BodyPart  string
	Study     string
	Slice     int
	Size      int
}

// Bytes writes the slice as an uncompressed DICOM file.
func (s Synth) Bytes() ([]byte, error) {
	n := s.Size
	if n == 0 {
		n = 128
	}
	nf := frame.NewNativeFrame[uint16](16, n, n, n*n, 1)
	c := float64(n) / 2
	spotX := c + float64(n)/5*math.Cos(float64(s.Slice)/3)
	spotY := c + float64(n)/5*math.Sin(float64(s.Slice)/3)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-c, float64(y)-c)
			v := -1000.0 // air
			switch {
			case math.Hypot(float64(x)-spotX, float64(y)-spotY) < float64(n)/16:
				v = -800 // a dark spot
			case d < float64(n)*0.38:
				v = 40 // soft tissue
			case d < float64(n)*0.45:
				v = 700 // a bright ring, like bone
			}
			nf.RawData[y*n+x] = uint16(v + 1024) // stored with an intercept of -1024
		}
	}
	sop := fmt.Sprintf("%s.%d", s.SeriesUID, s.Slice+1)
	mk := func(t tag.Tag, v any) *dicom.Element {
		el, err := dicom.NewElement(t, v)
		if err != nil {
			panic(fmt.Sprintf("%v: %v", t, err))
		}
		return el
	}
	ds := dicom.Dataset{Elements: []*dicom.Element{
		mk(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.2"}),
		mk(tag.MediaStorageSOPInstanceUID, []string{sop}),
		mk(tag.TransferSyntaxUID, []string{"1.2.840.10008.1.2.1"}),
		mk(tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.2"}),
		mk(tag.SOPInstanceUID, []string{sop}),
		mk(tag.StudyDate, []string{s.Date}),
		mk(tag.Modality, []string{s.Modality}),
		mk(tag.InstitutionName, []string{"Example Imaging Center"}),
		mk(tag.StudyDescription, []string{s.Study}),
		mk(tag.SeriesDescription, []string{s.Series}),
		mk(tag.PatientName, []string{s.Patient}),
		mk(tag.BodyPartExamined, []string{s.BodyPart}),
		mk(tag.StudyInstanceUID, []string{s.StudyUID}),
		mk(tag.SeriesInstanceUID, []string{s.SeriesUID}),
		mk(tag.SeriesNumber, []string{fmt.Sprint(s.SeriesNum)}),
		mk(tag.InstanceNumber, []string{fmt.Sprint(s.Slice + 1)}),
		mk(tag.SliceLocation, []string{fmt.Sprint(float64(s.Slice) * 5)}),
		mk(tag.SamplesPerPixel, []int{1}),
		mk(tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mk(tag.Rows, []int{n}),
		mk(tag.Columns, []int{n}),
		mk(tag.BitsAllocated, []int{16}),
		mk(tag.BitsStored, []int{16}),
		mk(tag.HighBit, []int{15}),
		mk(tag.PixelRepresentation, []int{0}),
		mk(tag.WindowCenter, []string{"40"}),
		mk(tag.WindowWidth, []string{"400"}),
		mk(tag.RescaleIntercept, []string{"-1024"}),
		mk(tag.RescaleSlope, []string{"1"}),
		mk(tag.PixelData, dicom.PixelDataInfo{Frames: []*frame.Frame{{NativeData: nf}}}),
	}}
	var buf bytes.Buffer
	if err := dicom.Write(&buf, ds); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
