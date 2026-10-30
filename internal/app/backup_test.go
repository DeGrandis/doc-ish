package app

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// backupOf signs in to a fresh app with one file and one value, and
// downloads its backup.
func backupOf(t *testing.T) ([]byte, string) {
	t.Helper()
	srv, c, _ := testServer(t)
	var setup struct {
		Recovery string `json:"recovery"`
	}
	call(t, c, "POST", srv.URL+"/api/setup", map[string]string{"password": "correct horse", "name": "Jane Doe"}, &setup)
	uploadFiles(t, c, srv.URL+"/api/people/1/documents", map[string][]byte{"note.txt": []byte("Penicillin allergy: hives.")})
	call(t, c, "POST", srv.URL+"/api/people/1/observations", map[string]any{"name": "Weight", "value": 150, "unit": "lb"}, nil)

	if code := call(t, c, "POST", srv.URL+"/api/backup", map[string]string{"password": "wrong"}, nil); code != 403 {
		t.Fatalf("backup with the wrong password: %d", code)
	}
	b, _ := json.Marshal(map[string]string{"password": "correct horse"})
	resp, err := c.Post(srv.URL+"/api/backup", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	tarball, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(tarball) < 1024 {
		t.Fatalf("backup: %d, %d bytes", resp.StatusCode, len(tarball))
	}
	if bytes.Contains(tarball, []byte("Penicillin")) || bytes.Contains(tarball, []byte("Jane Doe")) {
		t.Fatal("the backup has readable records in it")
	}
	return tarball, setup.Recovery
}

func restoreInto(t *testing.T, tarball []byte, fields map[string]string) (*httptest.Server, *http.Client, int) {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), Version: "test", Web: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() { srv.Close(); a.Close() })
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", "backup.tar")
	fw.Write(tarball)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/restore", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return srv, c, resp.StatusCode
}

func TestBackupAndRestore(t *testing.T) {
	tarball, recovery := backupOf(t)

	if _, _, code := restoreInto(t, tarball, map[string]string{"password": "not it"}); code != 400 {
		t.Fatalf("restore with the wrong password: %d", code)
	}
	srv, c, code := restoreInto(t, tarball, map[string]string{"password": "correct horse"})
	if code != 200 {
		t.Fatalf("restore: %d", code)
	}
	var ps []map[string]any
	call(t, c, "GET", srv.URL+"/api/people", nil, &ps)
	var obs []map[string]any
	call(t, c, "GET", srv.URL+"/api/people/1/observations", nil, &obs)
	resp, _ := c.Get(srv.URL + "/api/documents/1/file")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(ps) != 1 || ps[0]["name"] != "Jane Doe" || len(obs) != 1 || !strings.Contains(string(body), "Penicillin") {
		t.Fatalf("restored: people %v, values %v, file %q", ps, obs, body)
	}
	// A second restore over records is refused.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	r2, _ := c.Post(srv.URL+"/api/restore", mw.FormDataContentType(), &buf)
	r2.Body.Close()
	if r2.StatusCode != 409 {
		t.Fatalf("restore over records: %d", r2.StatusCode)
	}

	// With only the recovery key, and a new password.
	srv, c, code = restoreInto(t, tarball, map[string]string{"recovery": strings.ToLower(recovery), "new_password": "a new password"})
	if code != 200 {
		t.Fatalf("restore with the recovery key: %d", code)
	}
	call(t, c, "POST", srv.URL+"/api/logout", nil, nil)
	if code := call(t, c, "POST", srv.URL+"/api/login", map[string]string{"password": "a new password"}, nil); code != 200 {
		t.Fatalf("login with the new password: %d", code)
	}
}

func TestAutomaticBackups(t *testing.T) {
	srv, c, a := testServer(t)
	call(t, c, "POST", srv.URL+"/api/setup", map[string]string{"password": "correct horse", "name": "Jane"}, nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "someone-elses.tar"), []byte("x"), 0o600)
	if code := call(t, c, "PUT", srv.URL+"/api/backups", map[string]any{"dir": dir, "keep": 2, "every_hours": 24}, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	if code := call(t, c, "PUT", srv.URL+"/api/backups", map[string]any{"dir": "relative/path"}, nil); code != 400 {
		t.Fatalf("relative folder accepted: %d", code)
	}
	// Three backups a minute apart (named by the minute), keeping two.
	for i := 0; i < 3; i++ {
		b := a.runAutoBackup()
		if b.LastError != "" {
			t.Fatal(b.LastError)
		}
		os.Rename(filepath.Join(dir, b.LastFile), filepath.Join(dir, strings.Replace(b.LastFile, "backup-", "backup-0"+string(rune('0'+i)), 1)))
		prune(dir, 2)
	}
	all, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(all) != 3 { // two backups, and the file that isn't ours
		t.Fatalf("after pruning: %v", all)
	}
	// Not due again for a day.
	before := a.loadAuto().Last
	a.backupIfDue()
	if a.loadAuto().Last != before {
		t.Fatal("ran again before it was due")
	}
}
