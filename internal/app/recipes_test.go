package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"
)

// meterCSV is an invented glucose meter export: device lines, a header,
// two readings a day.
func meterCSV(start time.Time, days int) []byte {
	var b strings.Builder
	b.WriteString("Glucose Meter Export\nDevice: Example GM-200\n\nDate,Time,Glucose (mg/dL),Meal\n")
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("01/02/2006")
		fmt.Fprintf(&b, "%s,07:05,%d,Before breakfast\n%s,19:40,%d,After dinner\n", d, 90+i%30, d, 120+i%40)
	}
	return []byte(b.String())
}

func upload(t *testing.T, c *http.Client, url, name string, body []byte) int64 {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(body)
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
	if len(res) != 1 || res[0].Document == nil {
		t.Fatalf("upload: %+v", res)
	}
	return res[0].Document.ID
}

type recipeDoc struct {
	docJSON
	ValueCount int `json:"value_count"`
	Recipe     *struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		Name    string `json:"name"`
		Preview struct {
			Total int `json:"total"`
		} `json:"preview"`
	} `json:"recipe"`
}

func TestRecipeDraftApproveReuseUndo(t *testing.T) {
	readAnswer := `{"kind":"device","title":"Glucose meter export","doc_date":"","provider":"","patient_name":"","summary":"Glucose readings.",
		"confidence":"high","questions":[],"medications":[],"observations":[{"name":"Glucose","value":90,"value_text":"","unit":"mg/dL","ref_low":null,"ref_high":null,"flag":"","date":"2026-01-01"}]}`
	recipe := `{"name":"Glucose meter export","kind":"device","description":"Two glucose readings a day.","script":` + jsonString(`
rows = csv_dicts(skip=3)
for r in rows:
    when = datetime(r["Date"] + " " + r["Time"])
    g = number(r["Glucose (mg/dL)"])
    if when == None or g == None:
        continue
    emit("Glucose", g, unit="mg/dL", date=when, note=r["Meal"])
`) + `}`
	o := &scriptedOllama{steps: []string{
		`{"content":` + jsonString(readAnswer) + `}`,
		`{"content":` + jsonString(`{"name":"broken","kind":"device","description":"x","script":"emit(\"Glucose\", 1)"}`) + `}`, // no date: fixed on the second try
		`{"content":` + jsonString(recipe) + `}`,
	}}
	srv, c := signedIn(t, o.server(t).URL)

	id := upload(t, c, srv.URL+"/api/people/1/documents", "meter-jan.csv", meterCSV(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 200))
	doc := srv.URL + "/api/documents/" + itoa(id)
	waitRead(t, c, doc)
	var d recipeDoc
	call(t, c, "GET", doc, nil, &d)
	if d.Status != "review" || d.Recipe == nil || d.Recipe.Status != "proposed" || d.Recipe.Preview.Total != 400 || d.ValueCount != 0 {
		t.Fatalf("draft: %+v recipe %+v", d, d.Recipe)
	}
	if len(o.seen) != 3 || !strings.Contains(o.seen[2], "didn't work") || !strings.Contains(o.seen[1], `csv_dicts(skip=3)`) {
		t.Fatalf("the failed draft wasn't sent back (%d calls)", len(o.seen))
	}

	if code := call(t, c, "POST", srv.URL+"/api/recipes/"+itoa(d.Recipe.ID)+"/approve", nil, &d); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	if d.Status != "done" || d.ValueCount != 400 || d.Kind != "device" {
		t.Fatalf("approved: %+v", d)
	}

	// The next export in the same format is read by the recipe alone.
	calls := len(o.seen)
	id2 := upload(t, c, srv.URL+"/api/people/1/documents", "meter-aug.csv", meterCSV(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 30))
	d2 := waitRead(t, c, srv.URL+"/api/documents/"+itoa(id2))
	if d2.Status != "done" || len(o.seen) != calls {
		t.Fatalf("second export: %+v, %d model calls", d2, len(o.seen)-calls)
	}
	var obs []map[string]any
	call(t, c, "GET", srv.URL+"/api/people/1/observations?name=Glucose", nil, &obs)
	if len(obs) != 460 {
		t.Fatalf("glucose readings: %d", len(obs))
	}

	// Undoing the approval takes its values back and reopens the draft.
	var acts []struct {
		ID      int64  `json:"id"`
		Summary string `json:"summary"`
		CanUndo bool   `json:"can_undo"`
	}
	call(t, c, "GET", srv.URL+"/api/people/1/activity", nil, &acts)
	var approval int64
	for _, a := range acts {
		if strings.HasPrefix(a.Summary, "Approved the import recipe") && a.CanUndo {
			approval = a.ID
		}
	}
	if approval == 0 {
		t.Fatalf("no undoable approval: %+v", acts)
	}
	if code := call(t, c, "POST", srv.URL+"/api/activity/"+itoa(approval)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	call(t, c, "GET", doc, nil, &d)
	if d.Status != "review" || d.ValueCount != 0 || d.Recipe == nil || d.Recipe.Status != "proposed" {
		t.Fatalf("after undo: %+v %+v", d, d.Recipe)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
