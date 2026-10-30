package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/imaging"
	"github.com/DeGrandis/mymedical/internal/ingest"
	"github.com/DeGrandis/mymedical/internal/ollama"
	"github.com/DeGrandis/mymedical/internal/store"
)

// Imaging: DICOM files are gathered into studies as they arrive, from one
// file, many, or a zip of a hospital CD. A study's document is read from
// its tags alone (no model), and the viewer draws its images.

// viewMaxDim is the largest side sent to the viewer.
const viewMaxDim = 2048

// studyKey is a study document's stand-in for a file hash, so the same
// study is found again instead of added twice.
func studyKey(uid string) string {
	h := sha256.Sum256([]byte("dicom-study:" + uid))
	return hex.EncodeToString(h[:])
}

// addDicom files one DICOM image in its study, making the study's document
// when it's the first image seen. It returns the study's document and
// whether the image was new.
func (a *App) addDicom(st *store.Store, files *blobs.Store, pid int64, data []byte, filename string) (int64, bool, error) {
	hdr, err := imaging.Header(data)
	if err != nil {
		return 0, false, err
	}
	blob, size, _, err := files.Put(bytes.NewReader(data))
	if err != nil {
		return 0, false, err
	}
	var docID int64
	var added bool
	err = st.Tx(func(tx *sql.Tx) error {
		study, err := store.StudyByUIDTx(tx, pid, hdr.StudyUID)
		if errors.Is(err, store.ErrNotFound) {
			d := store.Document{PersonID: pid, Blob: blob, Filename: filename, Mime: "application/dicom", Size: size,
				SHA256: studyKey(hdr.StudyUID), Kind: "imaging", Title: hdr.Title(), DocDate: hdr.StudyDate,
				Provider: hdr.Institution, Status: store.StatusDone}
			if d.ID, err = store.AddDocumentTx(tx, d); err != nil {
				return err
			}
			study = store.Study{DocumentID: d.ID, PersonID: pid, StudyUID: hdr.StudyUID, Date: hdr.StudyDate, Modality: hdr.Modality,
				Description: hdr.Study, BodyPart: hdr.BodyPart, Institution: hdr.Institution, PatientName: hdr.PatientName}
			if study.ID, err = store.AddStudyTx(tx, study); err != nil {
				return err
			}
			if err := checkStudyName(tx, st, &d, hdr); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		docID = study.DocumentID
		modality := hdr.Modality
		sid, err := store.SeriesIDTx(tx, study.ID, hdr.SeriesUID, hdr.SeriesNum, hdr.Series, modality)
		if err != nil {
			return err
		}
		added, err = store.AddInstanceTx(tx, store.Instance{SeriesID: sid, SOPUID: hdr.SOPUID, Number: hdr.InstanceNum,
			Position: hdr.Position, Frames: hdr.Frames, Rows: hdr.Rows, Cols: hdr.Cols, Blob: blob, Size: size})
		if err != nil {
			return err
		}
		return describeStudyTx(tx, study, docID)
	})
	if err != nil || !added {
		files.Delete(blob)
	}
	return docID, added, err
}

// checkStudyName holds a new study for a look when the name in it isn't
// the profile's.
func checkStudyName(tx *sql.Tx, st *store.Store, d *store.Document, hdr imaging.Info) error {
	p, err := store.PersonTx(tx, d.PersonID)
	if err != nil {
		return err
	}
	owner := ingest.Person{ID: p.ID, Name: p.Name, Born: p.Profile.Born}
	reasons, _ := ingest.Check(ingest.Result{Kind: "imaging", PatientName: hdr.PatientName, DocDate: hdr.StudyDate, Confidence: "high"},
		ingest.Content{}, owner, nil, time.Now())
	if len(reasons) == 0 {
		return nil
	}
	d.Status, d.ReviewReason = store.StatusReview, strings.Join(reasons, "\n")
	return store.UpdateDocumentTx(tx, *d)
}

// describeStudyTx keeps a study's document summary and size current as
// images arrive.
func describeStudyTx(tx *sql.Tx, study store.Study, docID int64) error {
	series, images, size, err := store.StudyCountsTx(tx, study.ID)
	if err != nil {
		return err
	}
	d, err := store.DocumentTx(tx, docID)
	if err != nil {
		return err
	}
	what := imaging.Modalities[strings.ToUpper(study.Modality)]
	if what == "" {
		what = "Imaging study"
	}
	d.Summary = fmt.Sprintf("%s: %d image%s in %d series", what, images, plural(images), series)
	if study.Institution != "" {
		d.Summary += ", from " + study.Institution
	}
	d.Summary += "."
	d.Size = size
	if _, err := tx.Exec(`UPDATE documents SET summary = ?, size = ? WHERE id = ?`, d.Summary, d.Size, docID); err != nil {
		return err
	}
	return nil
}

// fromZip files what's in a zip (a hospital CD, typically): DICOM images
// into their studies, and readable files (a PDF report, photos) as files
// of their own. Viewer programs and indexes on the CD are left out.
func (a *App) fromZip(st *store.Store, files *blobs.Store, pid int64, zipPath, zipName string) ([]uploadResult, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errors.New("that zip couldn't be opened")
	}
	defer zr.Close()
	var out []uploadResult
	studies := map[int64]*uploadResult{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.UncompressedSize64 == 0 || f.UncompressedSize64 > 1<<30 {
			continue
		}
		name := filepath.Base(f.Name)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<30))
		rc.Close()
		if err != nil {
			continue
		}
		if imaging.IsDICOM(data) {
			docID, added, err := a.addDicom(st, files, pid, data, zipName)
			if err != nil {
				continue // DICOMDIR and other non-image DICOM files
			}
			r := studies[docID]
			if r == nil {
				d, _ := st.Document(docID)
				r = &uploadResult{Filename: zipName + ": " + d.Title, Document: &d}
				studies[docID] = r
			}
			if added {
				r.Images++
			}
			continue
		}
		kind := sniff(name, data[:min(len(data), 512)])
		if cdFile(name) || !readableInZip(kind) {
			continue
		}
		res := uploadResult{Filename: name}
		blob, size, sum, err := files.Put(bytes.NewReader(data))
		if err != nil {
			continue
		}
		d, err := st.AddDocument(store.Document{PersonID: pid, Blob: blob, Filename: name, Mime: kind, Size: size, SHA256: sum})
		if err != nil {
			files.Delete(blob)
			res.Duplicate = errors.Is(err, store.ErrDuplicate)
			if !res.Duplicate {
				continue
			}
		}
		res.Document = &d
		out = append(out, res)
	}
	for _, r := range studies {
		d, _ := st.Document(r.Document.ID)
		r.Document = &d
		r.Duplicate = r.Images == 0
		out = append(out, *r)
	}
	if len(out) == 0 {
		return nil, errors.New("nothing in that zip could be read")
	}
	return out, nil
}

// cdFile reports a CD's own files: its index and its viewer's launcher.
func cdFile(name string) bool {
	switch strings.ToUpper(name) {
	case "DICOMDIR", "AUTORUN.INF", "LOCKFILE", "VERSION", "README.TXT", "LICENSE.TXT":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".dll", ".inf", ".ini", ".bat", ".cmd", ".sys", ".jar", ".app", ".htm", ".html", ".js", ".css":
		return true
	}
	return false
}

func readableInZip(kind string) bool {
	kind = strings.TrimSpace(strings.SplitN(kind, ";", 2)[0])
	switch {
	case kind == "application/pdf", strings.HasPrefix(kind, "image/") && kind != "image/svg+xml",
		kind == "text/plain", kind == "text/csv", strings.Contains(kind, "wordprocessingml"):
		return true
	}
	return false
}

// linkReports pairs each study without a report with the closest imaging
// report on the same profile, dated within three days of it.
func linkReports(st *store.Store, pid int64) {
	studies, err := st.Studies(pid)
	if err != nil {
		return
	}
	docs, err := st.Documents(pid, "", 0)
	if err != nil {
		return
	}
	for _, s := range studies {
		if s.ReportID != nil || s.Date == "" {
			continue
		}
		sd, err := time.Parse("2006-01-02", s.Date)
		if err != nil {
			continue
		}
		best, bestGap := int64(0), 4*24*time.Hour
		for _, d := range docs {
			if d.Kind != "imaging" || d.Mime == "application/dicom" || d.DocDate == "" || d.Status != store.StatusDone {
				continue
			}
			dd, err := time.Parse("2006-01-02", d.DocDate)
			if err != nil {
				continue
			}
			if gap := time.Duration(math.Abs(float64(dd.Sub(sd)))); gap < bestGap {
				best, bestGap = d.ID, gap
			}
		}
		if best != 0 {
			st.Tx(func(tx *sql.Tx) error { return store.SetStudyReportTx(tx, s.ID, best) })
		}
	}
}

// ---------- the assistant ----------

const lookInstructions = `You look at one medical image for a family's private records app and describe it for them in plain words. You are not a radiologist and this is not a diagnosis.
- Say what the image shows: the kind of scan, the body part and view.
- Say what stands out or looks unusual, if anything, and how sure you are. Images can mislead; say so when unsure.
- If a report is given, read the image together with it: say whether what you see fits what the report says, and point out what the report mentions. The report, written by a radiologist who saw every image, is what counts.
- One image is a single slice or view of a study with many; say what that can't show.
- End with one or two things worth asking their doctor, if any.
Be brief: short paragraphs or a few bullets.`

func (e *toolEnv) describeImage(id int64) (string, error) {
	d, err := e.ownDoc(id)
	if err != nil {
		return "", err
	}
	study, err := e.st.StudyOf(d.ID)
	if err != nil {
		return "", errors.New("that file isn't an imaging study")
	}
	model := e.set.visionModel()
	if model == "" {
		return "No model for images is chosen in Settings, so the image can't be looked at. Say so.", nil
	}
	// The middle image of the series with the most images: usually the
	// main one, and its middle is usually the most telling slice.
	best := -1
	for i, s := range study.Series {
		if best < 0 || len(s.Instances) > len(study.Series[best].Instances) {
			best = i
		}
	}
	if best < 0 {
		return "", errors.New("the study has no images")
	}
	mid, _ := middle(study.Series[best])
	in, _, err := e.st.Instance(mid.ID)
	if err != nil {
		return "", err
	}
	p, err := decodeInstance(context.Background(), e.files, in, 0)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, p.Shrink(1024).Render(0, 0), &jpeg.Options{Quality: 90}); err != nil {
		return "", err
	}
	ser := study.Series[best]
	var user strings.Builder
	fmt.Fprintf(&user, "%s, dated %s. This is image %d of %d in the series %q (%d series in the study).\n",
		titleOf(d), study.Date, len(ser.Instances)/2+1, len(ser.Instances), ser.Description, len(study.Series))
	if study.ReportID != nil {
		if r, err := e.st.Document(*study.ReportID); err == nil && strings.TrimSpace(r.Text) != "" {
			t := r.Text
			if len(t) > 4000 {
				t = strings.ToValidUTF8(t[:4000], "") + " […]"
			}
			fmt.Fprintf(&user, "\nThe radiology report:\n<<<\n%s\n>>>\n", t)
		}
	}
	c, err := e.set.chatter()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	text, _, err := c.Chat(ctx, ollama.ChatRequest{Model: model, NumCtx: e.set.NumCtx, Messages: []ollama.Message{
		{Role: "system", Content: lookInstructions},
		{Role: "user", Content: user.String(), Images: []string{base64.StdEncoding.EncodeToString(buf.Bytes())}},
	}}, nil)
	if err != nil {
		return "", fmt.Errorf("the image model: %w", err)
	}
	e.note("describe_image", "Looked at: "+titleOf(d), d.ID)
	return "What the image model saw (an AI's look at one image, not a radiologist's reading):\n" + strings.TrimSpace(text), nil
}

// ---------- endpoints ----------

// studyView is a study for the viewer, with its report's title.
type studyView struct {
	store.Study
	Report *struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	} `json:"report,omitempty"`
}

func (a *App) handleStudy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		s, err := st.StudyOf(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		v := studyView{Study: s}
		if s.ReportID != nil {
			if d, err := st.Document(*s.ReportID); err == nil {
				v.Report = &struct {
					ID    int64  `json:"id"`
					Title string `json:"title"`
				}{d.ID, titleOf(d)}
			}
		}
		writeJSON(w, v)
	})
}

// handleStudyReport links a study to a report, or unlinks it (0).
func (a *App) handleStudyReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req struct {
		ReportID int64 `json:"report_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		s, err := st.StudyByID(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if req.ReportID != 0 {
			if d, err := st.Document(req.ReportID); err != nil || d.PersonID != s.PersonID {
				writeErr(w, 400, "that report isn't on this profile")
				return
			}
		}
		if err := st.Tx(func(tx *sql.Tx) error { return store.SetStudyReportTx(tx, id, req.ReportID) }); err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// decodeInstance reads one image's pixels.
func decodeInstance(ctx context.Context, files *blobs.Store, in store.Instance, frame int) (*imaging.Pixels, error) {
	rc, err := files.Open(in.Blob)
	if err != nil {
		return nil, err
	}
	data, err := imaging.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}
	return imaging.Decode(data, frame, tmpDir())
}

// handlePixels sends one frame for the viewer: grayscale as 16-bit samples
// (window and level then change in the browser), color as a PNG. What the
// samples mean is in the X-Image header.
func (a *App) handlePixels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	frame, _ := strconv.Atoi(r.URL.Query().Get("frame"))
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		in, _, err := st.Instance(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		p, err := decodeInstance(r.Context(), files, in, frame)
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
		p = p.Shrink(viewMaxDim)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		if p.RGB != nil {
			meta, _ := json.Marshal(map[string]any{"rows": p.Rows, "cols": p.Cols, "color": true})
			w.Header().Set("X-Image", string(meta))
			w.Header().Set("Content-Type", "image/jpeg")
			jpeg.Encode(w, p.Render(0, 0), &jpeg.Options{Quality: 92})
			return
		}
		packed, raw := p.Pack()
		meta, _ := json.Marshal(packed)
		w.Header().Set("X-Image", string(meta))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(raw)
	})
}

// middle is the image in the middle of a series, the usual one to show.
func middle(s store.Series) (store.Instance, bool) {
	if len(s.Instances) == 0 {
		return store.Instance{}, false
	}
	return s.Instances[len(s.Instances)/2], true
}

// handleSeriesThumb draws a small picture of a series' middle image.
func (a *App) handleSeriesThumb(w http.ResponseWriter, r *http.Request) {
	docID, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("series"))
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		s, err := st.StudyOf(docID)
		if err != nil || n < 0 || n >= len(s.Series) {
			writeErr(w, 404, "no such series")
			return
		}
		mid, ok := middle(s.Series[n])
		if !ok {
			writeErr(w, 404, "the series has no images")
			return
		}
		in, _, err := st.Instance(mid.ID)
		if err != nil {
			storeErr(w, err)
			return
		}
		p, err := decodeInstance(r.Context(), files, in, 0)
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		jpeg.Encode(w, p.Shrink(160).Render(0, 0), &jpeg.Options{Quality: 80})
	})
}

// serveStudyZip downloads every image of a study as one zip.
func serveStudyZip(w http.ResponseWriter, st *store.Store, files *blobs.Store, d store.Document) {
	ins, err := st.StudyBlobs(d.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '-'
		}
		return r
	}, titleOf(d))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
	w.Header().Set("Cache-Control", "private, no-store")
	zw := zip.NewWriter(w)
	for _, in := range ins {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("series-%03d/image-%05d.dcm", in.SeriesID, in.Number), Method: zip.Store})
		if err != nil {
			break
		}
		rc, err := files.Open(in.Blob)
		if err != nil {
			log.Printf("study zip: %v", err)
			continue
		}
		io.Copy(fw, rc)
		rc.Close()
	}
	zw.Close()
}

// deleteStudyBlobs removes a study's image files (its document's own blob
// is one of them).
func deleteStudyBlobs(st *store.Store, files *blobs.Store, docID int64) {
	ins, err := st.StudyBlobs(docID)
	if err != nil {
		return
	}
	for _, in := range ins {
		files.Delete(in.Blob)
	}
}

// spool writes an upload part to a temporary file (for a zip, which needs
// to be read out of order).
func spool(r io.Reader, dir string) (string, error) {
	os.MkdirAll(dir, 0o700)
	f, err := os.CreateTemp(dir, "upload-*.zip")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(r, 4<<30))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
