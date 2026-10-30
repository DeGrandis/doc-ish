package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DeGrandis/doc-ish/internal/imaging"
)

func ctSlice(t *testing.T, series string, n int) []byte {
	t.Helper()
	b, err := imaging.Synth{Patient: "DOE^JANE", StudyUID: "9.9.9", SeriesUID: "9.9.9." + series, Series: "AXIAL " + series,
		SeriesNum: len(series), Date: "20260901", Modality: "CT", BodyPart: "CHEST", Study: "CT CHEST", Slice: n, Size: 32}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func uploadFiles(t *testing.T, c *http.Client, url string, files map[string][]byte) []uploadResult {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, b := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(b)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res []uploadResult
	json.NewDecoder(resp.Body).Decode(&res)
	return res
}

type studyJSON struct {
	ID     int64 `json:"id"`
	Series []struct {
		Description string `json:"description"`
		Instances   []struct {
			ID     int64 `json:"id"`
			Number int   `json:"number"`
		} `json:"instances"`
	} `json:"series"`
	Report *struct {
		ID int64 `json:"id"`
	} `json:"report"`
}

func TestImagingStudy(t *testing.T) {
	srv, c, a := signedInApp(t, "http://127.0.0.1:1")
	up := srv.URL + "/api/people/1/documents"

	// Two images in one upload, out of order: one study.
	res := uploadFiles(t, c, up, map[string][]byte{"IM0002": ctSlice(t, "1", 1), "IM0001": ctSlice(t, "1", 0)})
	if len(res) != 1 || res[0].Document == nil || res[0].Images != 2 || res[0].Document.Kind != "imaging" || res[0].Document.Status != "done" {
		t.Fatalf("first upload: %+v", res)
	}
	doc := res[0].Document.ID

	// The same image again, a new one, and a "CD" with a second series, a
	// report PDF and the CD's index and viewer program.
	uploadFiles(t, c, up, map[string][]byte{"IM0002": ctSlice(t, "1", 1), "IM0003": ctSlice(t, "1", 2)})
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, b := range map[string][]byte{
		"DICOM/S2/IM0001": ctSlice(t, "22", 0), "DICOM/S2/IM0002": ctSlice(t, "22", 1),
		"DICOMDIR": []byte("not an image"), "VIEWER.EXE": []byte("MZ binary"),
		"REPORT.TXT": []byte("CT chest report, 2026-09-01. Findings: a small nodule. Impression: follow-up in 6 months."),
	} {
		w, _ := zw.Create(name)
		w.Write(b)
	}
	zw.Close()
	res = uploadFiles(t, c, up, map[string][]byte{"cd.zip": zb.Bytes()})
	var gotStudy, gotReport bool
	for _, r := range res {
		gotStudy = gotStudy || (r.Document != nil && r.Document.ID == doc && r.Images == 2)
		gotReport = gotReport || r.Filename == "REPORT.TXT"
	}
	if !gotStudy || !gotReport || len(res) != 2 {
		t.Fatalf("zip: %+v", res)
	}

	var s studyJSON
	call(t, c, "GET", srv.URL+"/api/documents/"+itoa(doc)+"/study", nil, &s)
	if len(s.Series) != 2 || len(s.Series[0].Instances) != 3 || s.Series[0].Instances[0].Number != 1 || len(s.Series[1].Instances) != 2 {
		t.Fatalf("study: %+v", s)
	}
	var d docJSON
	call(t, c, "GET", srv.URL+"/api/documents/"+itoa(doc), nil, &d)
	if d.Title != "CT scan, chest (CT CHEST)" {
		t.Fatalf("title: %q", d.Title)
	}

	// Pixels: 16-bit samples with their meaning in X-Image.
	resp, err := c.Get(srv.URL + "/api/instances/" + itoa(s.Series[0].Instances[0].ID) + "/pixels")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var meta struct {
		Rows   int     `json:"rows"`
		Offset float64 `json:"offset"`
		Center float64 `json:"center"`
	}
	json.Unmarshal([]byte(resp.Header.Get("X-Image")), &meta)
	if resp.StatusCode != 200 || len(body) != 32*32*2 || meta.Rows != 32 || meta.Offset != -1000 || meta.Center != 40 {
		t.Fatalf("pixels: %d, %d bytes, %+v", resp.StatusCode, len(body), meta)
	}
	resp, _ = c.Get(srv.URL + "/api/documents/" + itoa(doc) + "/thumb?series=1")
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// Downloading the study gives every image.
	resp, _ = c.Get(srv.URL + "/api/documents/" + itoa(doc) + "/file?download=1")
	zbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zbody), int64(len(zbody)))
	if err != nil || len(zr.File) != 5 {
		t.Fatalf("download: %v, %d files", err, len(zr.File))
	}

	// Linking a report by hand, then deleting the study removes its files.
	var report int64
	for _, r := range res {
		if r.Filename == "REPORT.TXT" {
			report = r.Document.ID
		}
	}
	if code := call(t, c, "PUT", srv.URL+"/api/studies/"+itoa(s.ID)+"/report", map[string]int64{"report_id": report}, nil); code != 200 {
		t.Fatalf("link: %d", code)
	}
	call(t, c, "GET", srv.URL+"/api/documents/"+itoa(doc)+"/study", nil, &s)
	if s.Report == nil || s.Report.ID != report {
		t.Fatalf("report not linked: %+v", s.Report)
	}
	before := countFiles(t, a.cfg.DataDir)
	if code := call(t, c, "DELETE", srv.URL+"/api/documents/"+itoa(doc), nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if after := countFiles(t, a.cfg.DataDir); before-after != 5 {
		t.Fatalf("deleting the study removed %d files, want 5", before-after)
	}
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	filepath.Walk(filepath.Join(dir, "files"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".age") {
			n++
		}
		return nil
	})
	return n
}
