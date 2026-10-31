package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ollama's MLX engine without its grammar library refuses a format with
// 501; the schema goes into the instructions instead, and the answer's
// fence comes off.
func TestChatJSONWithoutStructuredOutput(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Format   json.RawMessage `json:"format"`
			Messages []Message       `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Format != nil {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprint(w, `{"error":"structured output is unavailable"}`)
			return
		}
		last := body.Messages[len(body.Messages)-1].Content
		if !strings.Contains(last, `"kind"`) {
			t.Errorf("schema not in the instructions: %q", last)
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"`+"```json\\n{\\\"kind\\\":\\\"lab\\\"}\\n```"+`"}}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string"}}}
	out, err := c.ChatJSON(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}, schema)
	if err != nil || out != `{"kind":"lab"}` || calls != 2 {
		t.Fatalf("%q, %v, %d calls", out, err, calls)
	}
}

// A caller that only wants the whole answer (describe_image) passes no
// onText.
func TestChatWithoutOnText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"A clear "}}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"lung."},"done":true}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := c.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
	if err != nil || text != "A clear lung." {
		t.Fatalf("%q, %v", text, err)
	}
}
