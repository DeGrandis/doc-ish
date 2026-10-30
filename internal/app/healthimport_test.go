package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/DeGrandis/doc-ish/internal/healthexport"
)

type activityJSON struct {
	ID      int64  `json:"id"`
	Summary string `json:"summary"`
	CanUndo bool   `json:"can_undo"`
}

func findActivity(t *testing.T, acts []activityJSON, prefix string) activityJSON {
	t.Helper()
	for _, a := range acts {
		if strings.HasPrefix(a.Summary, prefix) {
			return a
		}
	}
	t.Fatalf("no activity %q in %+v", prefix, acts)
	return activityJSON{}
}

func TestHealthExportImport(t *testing.T) {
	// No model reachable: exports don't need one.
	srv, c, _ := signedInApp(t, "http://127.0.0.1:1")
	up := srv.URL + "/api/people/1/documents"
	values := func(name string) []struct {
		Value    *float64 `json:"value"`
		Observed string   `json:"observed"`
		Note     string   `json:"note"`
	} {
		var out []struct {
			Value    *float64 `json:"value"`
			Observed string   `json:"observed"`
			Note     string   `json:"note"`
		}
		call(t, c, "GET", srv.URL+"/api/people/1/observations?name="+url.QueryEscape(name), nil, &out)
		return out
	}
	profile := func() string {
		var p struct {
			Profile map[string]any `json:"profile"`
		}
		call(t, c, "GET", srv.URL+"/api/people/1", nil, &p)
		var b strings.Builder
		for _, k := range []string{"born", "blood_type", "allergies", "conditions", "medications", "vaccinations"} {
			v, _ := json.Marshal(p.Profile[k])
			b.WriteString(k + "=" + strings.ReplaceAll(strings.ToLower(string(v)), `"`, "") + " ")
		}
		return b.String()
	}

	// The records in it are Jordan Example's, and this is Jane Doe: Review.
	res := uploadFiles(t, c, up, map[string][]byte{"export.zip": healthexport.InventedApple()})
	if len(res) != 1 || res[0].Document == nil {
		t.Fatalf("upload: %+v", res)
	}
	doc := srv.URL + "/api/documents/" + itoa(res[0].Document.ID)
	d := waitRead(t, c, doc)
	if d.Status != "review" || !strings.Contains(d.ReviewReason, "Jordan Example") || len(values("Steps")) != 0 {
		t.Fatalf("should wait for a look: %+v", d)
	}

	// Accepting it imports it.
	call(t, c, "POST", doc+"/accept", nil, nil)
	d = waitRead(t, c, doc)
	if d.Status != "done" || d.Kind != "device" || d.Title != "Apple Health export" {
		t.Fatalf("after accepting: %+v", d)
	}
	steps := values("Steps")
	if len(steps) != 92 || steps[62].Observed != "2026-09-01" || *steps[62].Value != 10000 {
		t.Fatalf("steps: %d %+v", len(steps), steps[62])
	}
	if bp := values("Blood pressure systolic"); len(bp) != 93 {
		t.Fatalf("blood pressure: %d", len(bp))
	}
	p := profile()
	for _, want := range []string{"born=1979-06-02", "blood_type=a+", "latex", "iron deficiency anemia", "ferrous sulfate", "influenza", "tdap"} {
		if !strings.Contains(p, want) {
			t.Fatalf("profile lacks %s: %s", want, p)
		}
	}
	if strings.Contains(p, "bronchitis") || strings.Contains(p, "azithromycin") {
		t.Fatalf("resolved and finished records added: %s", p)
	}

	// export.xml on its own: everything in it is already on record.
	zr, _ := zip.NewReader(bytes.NewReader(healthexport.InventedApple()), int64(len(healthexport.InventedApple())))
	var xmlBytes []byte
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "export.xml") {
			rc, _ := f.Open()
			xmlBytes, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	res = uploadFiles(t, c, up, map[string][]byte{"export.xml": xmlBytes})
	d = waitRead(t, c, srv.URL+"/api/documents/"+itoa(res[0].Document.ID))
	if d.Status != "done" || d.Kind != "device" || len(d.Values) != 0 || len(values("Steps")) != 92 {
		t.Fatalf("export.xml again: %+v, %d steps", d, len(values("Steps")))
	}

	// Google Fit's Takeout on the same profile.
	res = uploadFiles(t, c, up, map[string][]byte{"takeout.zip": healthexport.InventedGoogle()})
	gdoc := srv.URL + "/api/documents/" + itoa(res[0].Document.ID)
	if d = waitRead(t, c, gdoc); d.Status != "done" || d.Title != "Google Fit export" || len(values("Distance")) != 30 {
		t.Fatalf("google: %+v", d)
	}

	// Undo takes an import back out, and what it added to the profile.
	var acts []activityJSON
	call(t, c, "GET", srv.URL+"/api/people/1/activity", nil, &acts)
	call(t, c, "POST", srv.URL+"/api/activity/"+itoa(findActivity(t, acts, "Imported takeout.zip").ID)+"/undo", nil, nil)
	if len(values("Distance")) != 0 || len(values("Steps")) != 92 {
		t.Fatalf("after undoing Google: %d distances", len(values("Distance")))
	}
	call(t, c, "POST", srv.URL+"/api/activity/"+itoa(findActivity(t, acts, "Imported export.zip").ID)+"/undo", nil, nil)
	if len(values("Steps")) != 0 || strings.Contains(profile(), "latex") {
		t.Fatalf("after undoing Apple: %d steps, %s", len(values("Steps")), profile())
	}
}
