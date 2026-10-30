package ingest

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Dicom is what a DICOM file says about itself. No model is needed: the
// tags name the patient, date, modality and body part.
type Dicom struct {
	PatientName string `json:"patient_name,omitempty"`
	PatientBorn string `json:"patient_born,omitempty"`
	StudyDate   string `json:"study_date,omitempty"`
	Modality    string `json:"modality,omitempty"`
	BodyPart    string `json:"body_part,omitempty"`
	Study       string `json:"study,omitempty"`
	Series      string `json:"series,omitempty"`
	Institution string `json:"institution,omitempty"`
	StudyUID    string `json:"study_uid,omitempty"`
}

func extractDicom(r io.Reader) (Content, error) {
	b, _, err := readCapped(io.LimitReader(r, 1<<30))
	if err != nil {
		return Content{}, err
	}
	ds, err := dicom.ParseUntilEOF(bytes.NewReader(b), nil, dicom.SkipPixelData())
	if err != nil {
		return Content{}, errors.New("this DICOM file couldn't be read")
	}
	get := func(t tag.Tag) string {
		el, err := ds.FindElementByTag(t)
		if err != nil {
			return ""
		}
		if v, ok := el.Value.GetValue().([]string); ok {
			return strings.TrimSpace(strings.Join(v, " "))
		}
		return ""
	}
	d := &Dicom{
		PatientName: strings.Join(strings.FieldsFunc(get(tag.PatientName), func(r rune) bool { return r == '^' }), " "),
		PatientBorn: dicomDate(get(tag.PatientBirthDate)),
		StudyDate:   dicomDate(get(tag.StudyDate)),
		Modality:    get(tag.Modality),
		BodyPart:    get(tag.BodyPartExamined),
		Study:       get(tag.StudyDescription),
		Series:      get(tag.SeriesDescription),
		Institution: get(tag.InstitutionName),
		StudyUID:    get(tag.StudyInstanceUID),
	}
	return Content{Dicom: d, Extractor: "dicom"}, nil
}

// dicomDate turns 20240315 into 2024-03-15.
func dicomDate(s string) string {
	t, err := time.Parse("20060102", strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// modalities names the common imaging types.
var modalities = map[string]string{
	"CR": "X-ray", "DX": "X-ray", "MG": "Mammogram", "CT": "CT scan", "MR": "MRI", "US": "Ultrasound",
	"NM": "Nuclear medicine scan", "PT": "PET scan", "XA": "Angiogram", "RF": "Fluoroscopy", "OT": "Image",
}

// dicomResult describes a DICOM image from its tags alone.
func dicomResult(d *Dicom) Result {
	what := modalities[strings.ToUpper(d.Modality)]
	if what == "" {
		what = "Medical image"
	}
	title := what
	if d.BodyPart != "" {
		title += ", " + strings.ToLower(d.BodyPart)
	}
	if d.Study != "" && !strings.EqualFold(d.Study, d.BodyPart) {
		title += " (" + d.Study + ")"
	}
	sum := what
	if d.Series != "" {
		sum += ", series " + d.Series
	}
	if d.Institution != "" {
		sum += ", from " + d.Institution
	}
	return Result{
		Kind: "imaging", Title: title, DocDate: d.StudyDate, Provider: d.Institution,
		PatientName: d.PatientName, Summary: sum + ".", Confidence: "high",
	}
}
