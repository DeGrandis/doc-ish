package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DeGrandis/mymedical/internal/store"
)

// scriptedOllama answers chat calls in turn: each step is either tool
// calls or text. It records what the model was sent.
type scriptedOllama struct {
	mu    sync.Mutex
	steps []string // JSON for message: {"tool_calls":[...]} or {"content":"..."}
	seen  []string
}

func (s *scriptedOllama) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, string(body))
		step := `{"content":"done"}`
		if len(s.steps) > 0 {
			step, s.steps = s.steps[0], s.steps[1:]
		}
		s.mu.Unlock()
		var msg map[string]any
		json.Unmarshal([]byte(step), &msg)
		msg["role"] = "assistant"
		json.NewEncoder(w).Encode(map[string]any{"message": msg, "done": false})
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": ""}, "done": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type chatEvent struct {
	ChatID int64      `json:"chat_id"`
	T      string     `json:"t"`
	Tool   *toolUse   `json:"tool"`
	Chart  *chartSpec `json:"chart"`
	Change *change    `json:"change"`
	Open   int64      `json:"open"`
	Error  string     `json:"error"`
	Done   bool       `json:"done"`
}

func chat(t *testing.T, c *http.Client, url, message string, viewing int64) []chatEvent {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"message": message, "viewing": viewing})
	resp, err := c.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []chatEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev chatEvent
		json.Unmarshal(sc.Bytes(), &ev)
		if ev.Error != "" {
			t.Fatalf("chat error: %s", ev.Error)
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestChatSavesWeightWithUndo(t *testing.T) {
	o := &scriptedOllama{steps: []string{
		`{"tool_calls":[{"function":{"name":"note_value","arguments":{"name":"Weight","value":182,"unit":"lb"}}}]}`,
		`{"content":"Saved your weight, 182 lb."}`,
	}}
	srv, c := signedIn(t, o.server(t).URL)
	evs := chat(t, c, srv.URL+"/api/people/1/chat", "weight 182 today", 0)

	var ch *change
	var text string
	for _, ev := range evs {
		if ev.Change != nil {
			ch = ev.Change
		}
		text += ev.T
	}
	if ch == nil || !strings.HasPrefix(ch.Summary, "Noted weight 182 lb") || text != "Saved your weight, 182 lb." {
		t.Fatalf("events: %+v", evs)
	}
	// The tool's result went back to the model.
	if len(o.seen) != 2 || !strings.Contains(o.seen[1], `"role":"tool"`) || !strings.Contains(o.seen[1], "Saved.") {
		t.Fatalf("second call: %s", o.seen[len(o.seen)-1])
	}

	var obs []store.Observation
	call(t, c, "GET", srv.URL+"/api/people/1/observations?name=weight", nil, &obs)
	if len(obs) != 1 || *obs[0].Value != 182 || obs[0].Source != "chat" || obs[0].Observed != time.Now().Format("2006-01-02") {
		t.Fatalf("weight: %+v", obs)
	}

	// The chat keeps the change, so a reload can show its Undo button.
	var msgs []store.Message
	call(t, c, "GET", srv.URL+"/api/chats/1", nil, &msgs)
	if len(msgs) != 2 || !strings.Contains(string(msgs[1].Extra), `"changes"`) {
		t.Fatalf("messages: %+v", msgs)
	}

	if code := call(t, c, "POST", srv.URL+"/api/activity/"+itoa(ch.ActivityID)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	call(t, c, "GET", srv.URL+"/api/people/1/observations?name=weight", nil, &obs)
	if len(obs) != 0 {
		t.Fatalf("weight still there after undo: %+v", obs)
	}
}

func TestChatUpdatesProfile(t *testing.T) {
	o := &scriptedOllama{steps: []string{
		`{"tool_calls":[
		   {"function":{"name":"update_profile","arguments":{"field":"allergies","action":"add","name":"Penicillin","detail":"hives"}}},
		   {"function":{"name":"update_profile","arguments":"{\"field\":\"born\",\"action\":\"set\",\"value\":\"1985-04-12\"}"}}]}`,
		`{"content":"Got it."}`,
	}}
	srv, c := signedIn(t, o.server(t).URL)
	chat(t, c, srv.URL+"/api/people/1/chat", "I'm allergic to penicillin, I get hives. Born April 12 1985.", 0)

	var p store.Person
	call(t, c, "GET", srv.URL+"/api/people/1", nil, &p)
	if p.Profile.Born != "1985-04-12" || len(p.Profile.Allergies) != 1 || p.Profile.Allergies[0].Detail != "hives" {
		t.Fatalf("profile: %+v", p.Profile)
	}
}

func TestChartAndOpen(t *testing.T) {
	o := &scriptedOllama{}
	srv, c := signedIn(t, fakeOllama(t, "Jane Doe").URL)
	id := uploadCSV(t, c, srv.URL+"/api/people/1/documents")
	waitRead(t, c, srv.URL+"/api/documents/"+itoa(id))
	for date, v := range map[string]float64{"2025-03-01": 150, "2026-03-01": 146} {
		call(t, c, "POST", srv.URL+"/api/people/1/observations", map[string]any{"name": "LDL Cholesterol", "value": v, "unit": "mg/dL", "observed": date}, nil)
	}

	o.steps = []string{
		`{"tool_calls":[{"function":{"name":"chart","arguments":{"names":["ldl"]}}},{"function":{"name":"open_file","arguments":{"id":` + itoa(id) + `}}}]}`,
		`{"content":"LDL went from 150 to 142."}`,
	}
	call(t, c, "PUT", srv.URL+"/api/settings", map[string]any{"ollama_url": o.server(t).URL, "chat_model": "fake", "num_ctx": 16384}, nil)
	evs := chat(t, c, srv.URL+"/api/people/1/chat", "how is my LDL trending? show me the lab", id)

	var spec *chartSpec
	var opened int64
	for _, ev := range evs {
		if ev.Chart != nil {
			spec = ev.Chart
		}
		if ev.Open != 0 {
			opened = ev.Open
		}
	}
	if spec == nil || len(spec.Series) != 1 || spec.Series[0].Name != "LDL Cholesterol" || len(spec.Series[0].Points) != 3 {
		t.Fatalf("chart: %+v", spec)
	}
	if spec.Series[0].RefHigh == nil || *spec.Series[0].RefHigh != 99 || !spec.Series[0].Points[2].Out {
		t.Fatalf("chart range and flags: %+v", spec.Series[0])
	}
	if opened != id {
		t.Fatalf("opened %d, want %d", opened, id)
	}
	// The open file was described to the model.
	if !strings.Contains(o.seen[0], "What the user is looking at") {
		t.Fatal("the viewed file wasn't in the prompt")
	}
}

func TestNormalizeBloodType(t *testing.T) {
	for in, want := range map[string]string{"O positive": "O+", "ab neg": "AB−", "A+": "A+", "b-": "B−", "I don't know": "Don't know", "": ""} {
		if got := normalizeBloodType(in); got != want {
			t.Errorf("normalizeBloodType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOtherProfilesFilesAreOffLimits(t *testing.T) {
	srv, c, a := signedInApp(t, fakeOllama(t, "Jane Doe").URL)
	call(t, c, "POST", srv.URL+"/api/people", map[string]string{"name": "Sam Doe"}, nil)
	id := uploadCSV(t, c, srv.URL+"/api/people/1/documents")
	waitRead(t, c, srv.URL+"/api/documents/"+itoa(id))

	a.mu.RLock()
	env := &toolEnv{st: a.st, pid: 2, today: time.Now(), emit: func(any) {}, extra: &msgExtra{}}
	out, err := env.readFile(id)
	a.mu.RUnlock()
	if err == nil || strings.Contains(out, "LDL") {
		t.Fatalf("Sam's assistant read Jane's file: %q %v", out, err)
	}
}
