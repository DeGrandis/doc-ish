package app

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testServer(t *testing.T) (*httptest.Server, *http.Client, *App) {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), Version: "test", Web: fstest.MapFS{
		"index.html": {Data: []byte("<title>__BRAND__</title>")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() { srv.Close(); a.Close() })
	jar, _ := cookiejar.New(nil)
	return srv, &http.Client{Jar: jar}, a
}

func call(t *testing.T, c *http.Client, method, url string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestSetupLoginUpload(t *testing.T) {
	srv, c, a := testServer(t)
	u := srv.URL

	if code := call(t, c, "GET", u+"/api/people", nil, nil); code != 401 {
		t.Fatalf("people before setup: %d", code)
	}
	var setup struct {
		Recovery string `json:"recovery"`
		Person   struct {
			ID int64 `json:"id"`
		} `json:"person"`
	}
	if code := call(t, c, "POST", u+"/api/setup", map[string]string{"password": "correct horse", "name": "Alex"}, &setup); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	if setup.Recovery == "" || setup.Person.ID == 0 {
		t.Fatalf("setup answer: %+v", setup)
	}

	// Upload a lab file to the first profile, then the same bytes again.
	upload := func() []uploadResult {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "labs.csv")
		fw.Write([]byte("test,value,unit\nA1C,5.4,%\n"))
		mw.Close()
		req, _ := http.NewRequest("POST", u+"/api/people/1/documents", &buf)
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
	res := upload()
	if len(res) != 1 || res[0].Document == nil || res[0].Document.Mime != "text/csv" {
		t.Fatalf("upload: %+v", res)
	}
	if again := upload(); len(again) != 1 || !again[0].Duplicate {
		t.Fatalf("duplicate not noticed: %+v", again)
	}
	resp, err := c.Get(u + "/api/documents/1/file")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "A1C,5.4") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("file: %q %v", body, resp.Header)
	}

	// Lock forgets the key; signing in again unlocks it.
	if code := call(t, c, "POST", u+"/api/lock", nil, nil); code != 200 {
		t.Fatalf("lock: %d", code)
	}
	if a.unlocked() {
		t.Fatal("still unlocked")
	}
	if code := call(t, c, "POST", u+"/api/login", map[string]string{"password": "wrong one"}, nil); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code := call(t, c, "POST", u+"/api/login", map[string]string{"password": "correct horse"}, nil); code != 200 {
		t.Fatalf("login: %d", code)
	}
	var docs []map[string]any
	if call(t, c, "GET", u+"/api/people/1/documents", nil, &docs); len(docs) != 1 {
		t.Fatalf("documents after unlock: %v", docs)
	}

	// The recovery key sets a new password.
	call(t, c, "POST", u+"/api/lock", nil, nil)
	if code := call(t, c, "POST", u+"/api/recover", map[string]string{"recovery": setup.Recovery, "password": "new password"}, nil); code != 200 {
		t.Fatalf("recover: %d", code)
	}
	call(t, c, "POST", u+"/api/logout", nil, nil)
	if code := call(t, c, "POST", u+"/api/login", map[string]string{"password": "new password"}, nil); code != 200 {
		t.Fatalf("login with new password: %d", code)
	}
}

func TestCrossSiteRefused(t *testing.T) {
	srv, c, _ := testServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/api/setup", strings.NewReader(`{"password":"12345678","name":"x"}`))
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-site setup: %d", resp.StatusCode)
	}
}

func TestIndexBrand(t *testing.T) {
	srv, c, _ := testServer(t)
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<title>Doc-ish</title>") {
		t.Fatalf("index: %s", b)
	}
}
