package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
