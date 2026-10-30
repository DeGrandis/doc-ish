package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DeGrandis/doc-ish/internal/store"
)

func TestShownDone(t *testing.T) {
	doc := int64(7)
	ev := []store.Evidence{
		{Date: "2025-09-12", Name: "Blood pressure systolic", Source: "document", DocumentID: &doc},
		{Date: "2026-03-12", Name: "Hemoglobin", Source: "document"},
		{Date: "2026-03-12", Name: "Ferritin", Source: "device"},
		{Date: "2026-04-01", Name: "X-ray, chest (XR CHEST PA)", Kind: "imaging"},
		{Date: "2026-10-01", Name: "Influenza vaccine", Source: "vaccination"},
	}
	for _, c := range []struct {
		what, want string
	}{
		{"Recheck blood count and iron studies", "Hemoglobin"},
		{"Follow up in six months", "Blood pressure systolic"},
		{"Repeat chest CT", ""}, // a chest X-ray isn't the CT
		{"Chest X-ray", "X-ray, chest (XR CHEST PA)"},
		{"Flu vaccine", "Influenza vaccine"},
		{"Bring the pill bottles", ""},
	} {
		e, ok := shownDone(store.FollowUp{What: c.what}, ev)
		if got := e.Name; ok != (c.want != "") || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.what, got, ok, c.want)
		}
	}
}

// fakeReader answers a visit note with two follow-ups and a lab report
// with the recheck one of them asked for.
func fakeReader(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answer := `{"kind":"visit","title":"Progress note","doc_date":"2025-09-12","provider":"","patient_name":"Jane Doe",
			"summary":"Iron deficiency.","confidence":"high","questions":[],"medications":[],"observations":[],
			"follow_ups":[{"what":"Recheck blood count and iron studies","when":"in about six months","due":""},
			              {"what":"Follow-up chest CT","when":"in 12 months","due":""}]}`
		if strings.Contains(string(body), "Hemoglobin") {
			answer = `{"kind":"lab","title":"Blood count","doc_date":"2026-03-12","provider":"","patient_name":"Jane Doe",
				"summary":"Better.","confidence":"high","questions":[],"medications":[],"follow_ups":[],
				"observations":[{"name":"Hemoglobin","value":12.6,"value_text":"","unit":"g/dL","ref_low":12,"ref_high":15.5,"flag":"","date":""}]}`
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "done": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func uploadText(t *testing.T, c *http.Client, url, name, text string) int64 {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(text))
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

func TestFollowUpsFiledClosedUndone(t *testing.T) {
	srv, c := signedIn(t, fakeReader(t).URL)
	people := srv.URL + "/api/people/1"
	note := uploadText(t, c, people+"/documents", "note.txt", "Progress note. Recheck blood count and iron studies in about six months.")
	waitRead(t, c, srv.URL+"/api/documents/"+itoa(note))

	var fs []store.FollowUp
	call(t, c, "GET", people+"/followups", nil, &fs)
	if len(fs) != 2 || fs[0].What != "Recheck blood count and iron studies" || fs[0].Due != "2026-03-12" || fs[1].Due != "2026-09-12" ||
		fs[0].From != "2025-09-12" || fs[0].Status != "open" {
		t.Fatalf("filed: %+v", fs)
	}

	// The labs six months later show the recheck done.
	labs := uploadText(t, c, people+"/documents", "labs.txt", "Hemoglobin 12.6 g/dL")
	waitRead(t, c, srv.URL+"/api/documents/"+itoa(labs))
	call(t, c, "GET", people+"/followups", nil, &fs)
	if len(fs) != 2 || fs[0].What != "Follow-up chest CT" || fs[0].Status != "open" ||
		fs[1].Status != "done" || fs[1].DoneDate != "2026-03-12" || !strings.Contains(fs[1].DoneNote, "Hemoglobin") {
		t.Fatalf("after the labs: %+v", fs)
	}

	var tl struct {
		Coming []store.FollowUp `json:"coming"`
		Events []timelineEvent  `json:"events"`
	}
	call(t, c, "GET", people+"/timeline", nil, &tl)
	if len(tl.Coming) != 1 || len(tl.Events) != 3 || tl.Events[0].Title != "Blood count" || tl.Events[1].Title != "Done: Recheck blood count and iron studies" ||
		tl.Events[2].Title != "Progress note" {
		t.Fatalf("timeline: %+v", tl)
	}

	// Undoing the labs' read opens the recheck again.
	var acts []struct {
		ID      int64  `json:"id"`
		Summary string `json:"summary"`
	}
	call(t, c, "GET", people+"/activity", nil, &acts)
	if !strings.HasPrefix(acts[0].Summary, "Read labs.txt") {
		t.Fatalf("activity: %+v", acts)
	}
	call(t, c, "POST", srv.URL+"/api/activity/"+itoa(acts[0].ID)+"/undo", nil, nil)
	call(t, c, "GET", people+"/followups", nil, &fs)
	if fs[0].Status != "open" || fs[1].Status != "open" {
		t.Fatalf("after undo: %+v", fs)
	}

	// Set aside by hand, and back.
	dismissed := "dismissed"
	var f store.FollowUp
	if code := call(t, c, "PATCH", srv.URL+"/api/followups/"+itoa(fs[1].ID), map[string]any{"status": dismissed}, &f); code != 200 || f.Status != dismissed {
		t.Fatalf("dismiss: %d %+v", code, f)
	}
	call(t, c, "GET", people+"/activity", nil, &acts)
	call(t, c, "POST", srv.URL+"/api/activity/"+itoa(acts[0].ID)+"/undo", nil, nil)
	call(t, c, "GET", people+"/followups", nil, &fs)
	if fs[1].Status != "open" {
		t.Fatalf("after undoing the dismissal: %+v", fs)
	}

	// Added by hand: "in three months" from today.
	if code := call(t, c, "POST", people+"/followups", map[string]string{"what": "Dentist", "when": "in three months"}, &f); code != 200 || f.Due == "" {
		t.Fatalf("add: %d %+v", code, f)
	}
}

// fakeRelease answers with a file of two records: a visit and a lab
// table of two days, as a long file's merged answer is.
func fakeRelease(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := `{"kind":"other","title":"Records release","doc_date":"2026-03-12","provider":"","patient_name":"Jane Doe",
			"summary":"Visits and labs.","confidence":"high","questions":[],"medications":[],
			"sections":[{"first":2,"last":3,"kind":"visit","title":"Visit note","date":"2025-09-12","provider":"","summary":"Tired."},
			            {"first":4,"last":7,"kind":"lab","title":"Lab results","date":"2026-03-12","provider":"","summary":""}],
			"observations":[
			  {"name":"Blood pressure","value":128,"value_text":"","unit":"mmHg","ref_low":null,"ref_high":null,"flag":"","date":"2025-09-12","section":1},
			  {"name":"Ferritin","value":18,"value_text":"","unit":"ng/mL","ref_low":30,"ref_high":400,"flag":"L","date":"2025-09-12","section":2},
			  {"name":"Ferritin","value":41,"value_text":"","unit":"ng/mL","ref_low":30,"ref_high":400,"flag":"","date":"2026-03-12","section":2}],
			"follow_ups":[{"what":"Recheck iron","when":"in six months","due":"","from":"","section":1}]}`
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "done": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSectionsFiledAndOnTheTimeline(t *testing.T) {
	srv, c := signedIn(t, fakeRelease(t).URL)
	people := srv.URL + "/api/people/1"
	id := uploadText(t, c, people+"/documents", "release.txt", "Records release")
	doc := srv.URL + "/api/documents/" + itoa(id)
	waitRead(t, c, doc)

	var d struct {
		Sections []store.Section     `json:"sections"`
		Values   []store.Observation `json:"values"`
	}
	call(t, c, "GET", doc, nil, &d)
	if len(d.Sections) != 2 || d.Sections[0].Title != "Visit note" || d.Sections[1].FirstPage != 4 || len(d.Values) != 3 {
		t.Fatalf("filed: %+v", d)
	}
	for _, v := range d.Values {
		want := d.Sections[1].ID
		if v.Name == "Blood pressure" {
			want = d.Sections[0].ID
		}
		if v.SectionID == nil || *v.SectionID != want {
			t.Errorf("%s %s in section %v", v.Name, v.Observed, v.SectionID)
		}
	}
	var fs []store.FollowUp
	call(t, c, "GET", people+"/followups", nil, &fs)
	if len(fs) != 1 || fs[0].From != "2025-09-12" || fs[0].SectionID == nil || *fs[0].SectionID != d.Sections[0].ID {
		t.Fatalf("follow-up: %+v", fs)
	}

	// The visit on its day, the labs once per day, and the file itself.
	var tl struct {
		Events []timelineEvent `json:"events"`
	}
	call(t, c, "GET", people+"/timeline", nil, &tl)
	var got []string
	for _, e := range tl.Events {
		got = append(got, fmt.Sprintf("%s %s %s p%d %d", e.Date, e.Type, e.Title, e.Page, len(e.Flags)))
	}
	want := []string{"2026-03-12 file Records release p0 0", "2026-03-12 followup Done: Recheck iron p0 0", "2026-03-12 section Lab results p4 0", "2025-09-12 section Visit note p2 0",
		"2025-09-12 section Lab results p4 1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("timeline:\n%s", strings.Join(got, "\n"))
	}

	// Read again starts over: no sections left behind.
	call(t, c, "POST", doc+"/read", nil, nil)
	waitRead(t, c, doc)
	call(t, c, "GET", doc, nil, &d)
	if len(d.Sections) != 2 {
		t.Fatalf("read again: %d sections", len(d.Sections))
	}
	var acts []struct {
		ID      int64  `json:"id"`
		Summary string `json:"summary"`
	}
	call(t, c, "GET", people+"/activity", nil, &acts)
	call(t, c, "POST", srv.URL+"/api/activity/"+itoa(acts[0].ID)+"/undo", nil, nil)
	d.Sections = nil
	call(t, c, "GET", doc, nil, &d)
	if len(d.Sections) != 0 {
		t.Fatalf("after undo: %+v", d.Sections)
	}
}
