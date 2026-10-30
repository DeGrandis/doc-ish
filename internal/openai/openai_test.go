package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DeGrandis/mymedical/internal/ollama"
)

// fakeProvider streams a tool call in pieces, as providers do, and answers
// structured requests whole.
func fakeProvider(t *testing.T, seen *[]map[string]any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"No auth credentials found"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"b/text","supported_parameters":["tools"],"architecture":{"input_modalities":["text"]}},
				{"id":"a/vision","supported_parameters":["tools","structured_outputs"],"architecture":{"input_modalities":["text","image"]}}]}`)
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(b, &body)
			*seen = append(*seen, body)
			if body["response_format"] != nil {
				fmt.Fprint(w, "{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"kind\\\":\\\"lab\\\"}\\n```\"}}]}")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []string{
				`{"choices":[{"delta":{"content":"Let me "}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"note_value","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"name\":\"Wei"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ght\",\"value\":180}"}}]}}]}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			fmt.Fprint(w, ": keep-alive\n\ndata: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatToolCallsAndJSON(t *testing.T) {
	var seen []map[string]any
	srv := fakeProvider(t, &seen)
	c, err := New(srv.URL+"/v1/", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	text, calls, err := c.Chat(context.Background(), ollama.ChatRequest{Model: "a/vision", Messages: []ollama.Message{
		{Role: "user", Content: "weight 180", Images: []string{"aGk="}},
	}}, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if text != "Let me " || streamed.String() != text || len(calls) != 1 {
		t.Fatalf("text %q calls %+v", text, calls)
	}
	if calls[0].ID != "call_abc" || calls[0].Function.Name != "note_value" || string(calls[0].Function.Arguments) != `{"name":"Weight","value":180}` {
		t.Fatalf("call: %+v %s", calls[0], calls[0].Function.Arguments)
	}
	// The image went as a data URL part.
	parts, _ := seen[0]["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 || !strings.Contains(fmt.Sprint(parts[1]), "data:image/jpeg;base64,aGk=") {
		t.Fatalf("image parts: %v", parts)
	}

	// A follow-up quotes the call's id with its result.
	msgs := toMessages([]ollama.Message{
		{Role: "user", Content: "weight 180"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolName: "note_value", Content: "Saved."},
	})
	if msgs[1].ToolCalls[0].ID != "call_abc" || msgs[2].ToolCallID != "call_abc" || msgs[1].Content != nil {
		t.Fatalf("follow-up: %+v", msgs)
	}

	out, err := c.ChatJSON(context.Background(), ollama.ChatRequest{Model: "a/vision", Messages: []ollama.Message{{Role: "user", Content: "x"}}},
		map[string]any{"type": "object"})
	if err != nil || out != `{"kind":"lab"}` {
		t.Fatalf("json: %q %v", out, err)
	}
	rf := seen[len(seen)-1]["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format: %v", rf)
	}
}

func TestModelsAndErrors(t *testing.T) {
	var seen []map[string]any
	srv := fakeProvider(t, &seen)
	c, _ := New(srv.URL+"/v1", "test-key")
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 2 || models[0].Name != "a/vision" || !models[0].Vision || models[1].Vision || !models[1].Tools {
		t.Fatalf("models: %+v %v", models, err)
	}
	bad, _ := New(srv.URL+"/v1", "wrong")
	if _, err := bad.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "didn't accept the API key") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := New("http://example.com/v1", "k"); err == nil {
		t.Fatal("a key would go over plain http to the internet")
	}
}
