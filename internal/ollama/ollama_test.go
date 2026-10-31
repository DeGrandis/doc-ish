package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
