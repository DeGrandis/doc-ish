package app

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeOllama answers every read with the same JSON, naming patient.
func fakeOllama(t *testing.T, patient string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "LDL Cholesterol,142") {
			t.Errorf("the file's text wasn't sent: %s", body)
		}
		answer := `{"kind":"lab","title":"Lipid panel","doc_date":"2026-09-15","provider":"Test Lab","patient_name":"` + patient + `",
			"summary":"Cholesterol results.","confidence":"high","questions":[],"medications":[],
			"observations":[
			  {"name":"LDL Cholesterol","value":142,"value_text":"","unit":"mg/dL","ref_low":0,"ref_high":99,"flag":"H","date":""},
			  {"name":"HDL Cholesterol","value":48,"value_text":"","unit":"mg/dL","ref_low":40,"ref_high":null,"flag":"","date":""}]}`
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "done": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func uploadCSV(t *testing.T, c *http.Client, url string) int64 {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "lipids.csv")
	fw.Write([]byte("test,value,unit,low,high\nLDL Cholesterol,142,mg/dL,0,99\nHDL Cholesterol,48,mg/dL,40,\n"))
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

type docJSON struct {
	Status       string `json:"status"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	PersonID     int64  `json:"person_id"`
	ReviewReason string `json:"review_reason"`
	Values       []struct {
		Name     string `json:"name"`
		PersonID int64  `json:"person_id"`
	} `json:"values"`
}

func waitRead(t *testing.T, c *http.Client, url string) docJSON {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var d docJSON
		call(t, c, "GET", url, nil, &d)
		if d.Status != "new" && d.Status != "reading" {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %s", d.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func signedIn(t *testing.T, ollamaURL string) (*httptest.Server, *http.Client) {
	srv, c, _ := signedInApp(t, ollamaURL)
	return srv, c
}

func signedInApp(t *testing.T, ollamaURL string) (*httptest.Server, *http.Client, *App) {
	srv, c, a := testServer(t)
	if code := call(t, c, "POST", srv.URL+"/api/setup", map[string]string{"password": "correct horse", "name": "Jane Doe"}, nil); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	if code := call(t, c, "PUT", srv.URL+"/api/settings", map[string]any{"ollama_url": ollamaURL, "chat_model": "fake", "num_ctx": 16384}, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	return srv, c, a
}

func TestReaderFilesAndUndo(t *testing.T) {
	srv, c := signedIn(t, fakeOllama(t, "DOE, JANE").URL)
	id := uploadCSV(t, c, srv.URL+"/api/people/1/documents")
	doc := srv.URL + "/api/documents/" + itoa(id)
	d := waitRead(t, c, doc)
	if d.Status != "done" || d.Kind != "lab" || d.Title != "Lipid panel" || len(d.Values) != 2 {
		t.Fatalf("after reading: %+v", d)
	}

	var acts []struct {
		ID      int64  `json:"id"`
		Summary string `json:"summary"`
		CanUndo bool   `json:"can_undo"`
	}
	call(t, c, "GET", srv.URL+"/api/people/1/activity", nil, &acts)
	if len(acts) < 2 || !strings.HasPrefix(acts[0].Summary, "Read lipids.csv") || !acts[0].CanUndo {
		t.Fatalf("activity: %+v", acts)
	}
	if code := call(t, c, "POST", srv.URL+"/api/activity/"+itoa(acts[0].ID)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	call(t, c, "GET", doc, nil, &d)
	if d.Status != "done" || d.Kind != "" || len(d.Values) != 0 {
		t.Fatalf("after undo: %+v", d)
	}
	if code := call(t, c, "POST", srv.URL+"/api/activity/"+itoa(acts[0].ID)+"/undo", nil, nil); code != 400 {
		t.Fatalf("second undo: %d", code)
	}

	// Read again files it again.
	call(t, c, "POST", doc+"/read", nil, nil)
	if d = waitRead(t, c, doc); d.Status != "done" || len(d.Values) != 2 {
		t.Fatalf("read again: %+v", d)
	}
}

func TestReaderHoldsSomeoneElsesFile(t *testing.T) {
	srv, c := signedIn(t, fakeOllama(t, "Sam Doe").URL)
	call(t, c, "POST", srv.URL+"/api/people", map[string]string{"name": "Sam Doe"}, nil)
	id := uploadCSV(t, c, srv.URL+"/api/people/1/documents")
	doc := srv.URL + "/api/documents/" + itoa(id)
	d := waitRead(t, c, doc)
	if d.Status != "review" || len(d.Values) != 0 || !strings.Contains(d.ReviewReason, "Sam Doe's") {
		t.Fatalf("not held for review: %+v", d)
	}
	var review []docJSON
	call(t, c, "GET", srv.URL+"/api/people/1/review", nil, &review)
	if len(review) != 1 {
		t.Fatalf("review list: %+v", review)
	}

	// Move it to Sam, then file it: the values are Sam's.
	call(t, c, "PATCH", doc, map[string]any{"person_id": 2}, nil)
	if code := call(t, c, "POST", doc+"/accept", nil, &d); code != 200 {
		t.Fatalf("accept: %d", code)
	}
	if d.Status != "done" || d.PersonID != 2 || len(d.Values) != 2 || d.Values[0].PersonID != 2 {
		t.Fatalf("after accept: %+v", d)
	}
	var obs []map[string]any
	call(t, c, "GET", srv.URL+"/api/people/1/observations", nil, &obs)
	if len(obs) != 0 {
		t.Fatalf("Jane got Sam's values: %v", obs)
	}

	// Filing it came after the move, so only filing can be undone now.
	var acts []struct {
		Summary string `json:"summary"`
		CanUndo bool   `json:"can_undo"`
	}
	call(t, c, "GET", srv.URL+"/api/people/1/activity", nil, &acts)
	for _, a := range acts {
		if strings.HasPrefix(a.Summary, "Moved") && a.CanUndo {
			t.Fatalf("the move can still be undone under the filing: %+v", acts)
		}
	}
	call(t, c, "GET", srv.URL+"/api/people/2/activity", nil, &acts)
	if len(acts) == 0 || !strings.HasPrefix(acts[0].Summary, "Checked") || !acts[0].CanUndo {
		t.Fatalf("Sam's activity: %+v", acts)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
