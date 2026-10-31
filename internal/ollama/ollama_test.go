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

// A lab table runs about 1.4 characters to a token. These counts are
// what qwen3.5:9b reported for the same kind of text.
func TestTokens(t *testing.T) {
	for _, tc := range []struct {
		text string
		real int
	}{
		{strings.Repeat("- Ferritin: 123 ng/mL on 2025-09-12 (reference 12–150) [doc:123]\n", 100), 3932},
		{strings.Repeat("| 2025-09-12 | 45.3 | 123 | H |\n", 150), 4048},
	} {
		if got := Tokens(tc.text); got < tc.real*8/10 || got > tc.real*13/10 {
			t.Errorf("%d tokens for what the model counted as %d", got, tc.real)
		}
	}
}

func TestClip(t *testing.T) {
	text := strings.Repeat("| 2025-09-12 | 45.3 | 123 | H |\n", 100)
	short, cut := Clip(text, 500)
	if !cut || Tokens(short) > 510 || !strings.HasSuffix(short, "|") {
		t.Fatalf("cut %v, %d tokens", cut, Tokens(short))
	}
	if same, cut := Clip("short", 500); cut || same != "short" {
		t.Fatalf("%q, %v", same, cut)
	}
}
