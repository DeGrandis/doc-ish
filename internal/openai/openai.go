// Package openai runs the models on a hosted provider that speaks the
// OpenAI chat format (/v1/chat/completions): OpenRouter and many others,
// or a local server such as LM Studio or llama.cpp. It takes and returns
// the Ollama client's shapes (it is an ollama.Chatter), so the assistant,
// the reader and recipes work unchanged. Ported from Proof of Basis.
//
// Everything sent through it leaves this server, which is why the app only
// uses it after the user types their consent.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/DeGrandis/mymedical/internal/ollama"
)

type Client struct {
	base string // up to and including /v1, or the provider's equivalent
	key  string
	http *http.Client
}

// New checks the address and returns a client for it. A key is only sent
// over https, or plain http to this computer or the local network.
func New(base, key string) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/chat/completions")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("enter the provider's address like https://openrouter.ai/api/v1")
	}
	if u.Scheme == "http" && !local(u.Hostname()) {
		return nil, errors.New("use the provider's https address, so your key and data aren't sent in the clear")
	}
	return &Client{base: base, key: strings.TrimSpace(key), http: &http.Client{}}, nil
}

// local reports whether a host is this computer or on the local network.
func local(host string) bool {
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return false
		}
	}
	return true
}

// Host is the address's host, for messages.
func (c *Client) Host() string {
	u, _ := url.Parse(c.base)
	return u.Hostname()
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, friendly(err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

// statusError is a provider's refusal, kept so a caller can tell what was
// refused.
type statusError struct {
	status int
	detail string
	error
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := errorMessage(b)
	var err error
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		err = withDetail("the provider didn't accept the API key", msg, "")
	case resp.StatusCode == 402:
		err = withDetail("the provider says the account is out of credit", msg, "")
	case resp.StatusCode == 404:
		err = withDetail("the provider doesn't know that address or model", msg, "")
	case resp.StatusCode == 429:
		err = withDetail("the provider is limiting requests", msg, "; try again in a minute")
	case resp.StatusCode >= 500:
		err = withDetail("the provider had a problem", msg, "; try again shortly")
	case msg != "":
		err = fmt.Errorf("the provider: %s", msg)
	default:
		err = fmt.Errorf("the provider answered %s", resp.Status)
	}
	return &statusError{resp.StatusCode, msg, err}
}

// Limited reports whether err is the provider limiting requests, which is
// worth waiting out rather than calling the file unreadable.
func Limited(err error) bool {
	var se *statusError
	return errors.As(err, &se) && (se.status == 429 || se.status >= 500)
}

func refused(err error, statuses ...int) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	for _, st := range statuses {
		if se.status == st {
			return true
		}
	}
	return false
}

// errorMessage finds the message in an error body: {"error": {"message": …}}
// in the OpenAI format, {"error": "…"} or {"message": "…"} elsewhere.
func errorMessage(b []byte) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil && s != "" {
		return s
	}
	var o struct {
		Message  string
		Metadata struct{ Raw json.RawMessage } // OpenRouter: the upstream provider's own error
	}
	if json.Unmarshal(e.Error, &o) == nil && o.Message != "" {
		var raw string
		if json.Unmarshal(o.Metadata.Raw, &raw) == nil && raw != "" {
			if m := errorMessage([]byte(raw)); m != "" {
				raw = m
			}
			if len(raw) > 300 {
				raw = raw[:300] + "…"
			}
			return raw
		}
		return o.Message
	}
	return e.Message
}

func withDetail(what, detail, advice string) error {
	if detail != "" {
		what += " (" + detail + ")"
	}
	return errors.New(what + advice)
}

func friendly(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("the provider didn't answer in time")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("nothing is listening at that address")
	case strings.Contains(err.Error(), "no such host"):
		return errors.New("that host name doesn't resolve")
	}
	return fmt.Errorf("can't reach the provider: %v", err)
}

// Models lists the provider's models. Where the provider says what each
// can do (OpenRouter's supported_parameters and input_modalities), Tools
// and Vision are set from it; otherwise every model is offered for both.
func (c *Client) Models(ctx context.Context) ([]ollama.Model, error) {
	resp, err := c.request(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID           string   `json:"id"`
			Params       []string `json:"supported_parameters"`
			Architecture struct {
				Input []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("the provider's model list didn't read: %v", err)
	}
	out := make([]ollama.Model, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == "" {
			continue
		}
		om := ollama.Model{Name: m.ID, Tools: m.Params == nil, Vision: m.Architecture.Input == nil}
		for _, p := range m.Params {
			om.Tools = om.Tools || p == "tools"
		}
		for _, in := range m.Architecture.Input {
			om.Vision = om.Vision || in == "image"
		}
		out = append(out, om)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------- chat ----------

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // a string, a list of parts (text and images), or null
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON, as a string
	} `json:"function"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []message     `json:"messages"`
	Tools          []ollama.Tool `json:"tools,omitempty"`
	Stream         bool          `json:"stream"`
	Temperature    float64       `json:"temperature"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	ResponseFormat any           `json:"response_format,omitempty"`
}

// maxAnswerTokens matches the Ollama client's cap on an answer.
const maxAnswerTokens = 2000

// Chat streams one reply, returning its text and any tool calls.
func (c *Client) Chat(ctx context.Context, r ollama.ChatRequest, onText func(string)) (string, []ollama.ToolCall, error) {
	body := chatRequest{Model: r.Model, Messages: toMessages(r.Messages), Tools: r.Tools, Stream: true, Temperature: 0.2, MaxTokens: maxAnswerTokens}
	resp, err := c.request(ctx, "POST", "/chat/completions", body)
	if err != nil {
		if refused(err, 400, 404) && len(r.Tools) > 0 && strings.Contains(strings.ToLower(err.Error()), "tool") {
			return "", nil, ollama.ErrNoTools
		}
		return "", nil, err
	}
	defer resp.Body.Close()
	var text strings.Builder
	var calls []toolCall
	var other bytes.Buffer // a provider that answered without streaming
	streamed := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			if len(line) > 0 && line[0] != ':' && !bytes.HasPrefix(line, []byte("event:")) && !bytes.HasPrefix(line, []byte("id:")) && other.Len() < 8<<20 {
				other.Write(line)
				other.WriteByte('\n')
			}
			continue
		}
		streamed = true
		data = bytes.TrimSpace(data)
		if string(data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return text.String(), nil, fmt.Errorf("the provider sent something unexpected: %v", err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			if msg := errorMessage(data); msg != "" {
				return text.String(), nil, fmt.Errorf("the provider: %s", msg)
			}
			return text.String(), nil, errors.New("the provider stopped with an error")
		}
		for _, ch := range chunk.Choices {
			if d := ch.Delta.Content; d != "" {
				text.WriteString(d)
				if onText != nil {
					onText(d)
				}
			}
			// A call arrives in pieces: the id and name, then its
			// arguments a few characters at a time.
			for _, tc := range ch.Delta.ToolCalls {
				for len(calls) <= tc.Index {
					calls = append(calls, toolCall{Type: "function"})
				}
				t := &calls[tc.Index]
				if tc.ID != "" {
					t.ID = tc.ID
				}
				t.Function.Name += tc.Function.Name
				t.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return text.String(), nil, friendly(err)
	}
	if !streamed {
		var content string
		if content, calls, err = whole(other.Bytes()); err != nil {
			return "", nil, err
		}
		text.WriteString(content)
		if onText != nil && content != "" {
			onText(content)
		}
	}
	return text.String(), toOllamaCalls(calls), ctx.Err()
}

// ChatJSON asks for an answer matching schema, through the provider's
// structured outputs; one that doesn't have them gets JSON mode with the
// schema in the instructions.
func (c *Client) ChatJSON(ctx context.Context, r ollama.ChatRequest, schema any) (string, error) {
	body := chatRequest{Model: r.Model, Messages: toMessages(r.Messages), Temperature: 0,
		ResponseFormat: map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": schema}}}
	resp, err := c.request(ctx, "POST", "/chat/completions", body)
	if err != nil && refused(err, 400, 422) {
		s, _ := json.Marshal(schema)
		msgs := append([]ollama.Message{}, r.Messages...)
		msgs = append(msgs, ollama.Message{Role: "user", Content: "Answer with only a JSON object matching this JSON schema:\n" + string(s)})
		body.Messages, body.ResponseFormat = toMessages(msgs), map[string]string{"type": "json_object"}
		resp, err = c.request(ctx, "POST", "/chat/completions", body)
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", friendly(err)
	}
	content, _, err := whole(b)
	return strings.TrimSpace(stripFence(content)), err
}

// stripFence removes a ```json fence some models put around JSON.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s
}

func toOllamaCalls(calls []toolCall) []ollama.ToolCall {
	var out []ollama.ToolCall
	for i, t := range calls {
		if t.Function.Name == "" {
			continue
		}
		var tc ollama.ToolCall
		tc.ID = t.ID
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", i)
		}
		tc.Function.Name = t.Function.Name
		args := strings.TrimSpace(t.Function.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		tc.Function.Arguments = json.RawMessage(args)
		out = append(out, tc)
	}
	return out
}

// toMessages turns the app's messages into the OpenAI format: images
// become data URLs, each tool call keeps (or gets) an id, and each tool
// result answers the calls before it, in order.
func toMessages(in []ollama.Message) []message {
	out := make([]message, 0, len(in))
	var pending []string
	for i, m := range in {
		switch m.Role {
		case "assistant":
			msg := message{Role: "assistant"}
			pending = pending[:0]
			for j, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("call_app_%d_%d", i, j)
				}
				args := string(tc.Function.Arguments)
				if args == "" || args == "null" {
					args = "{}"
				}
				t := toolCall{ID: id, Type: "function"}
				t.Function.Name, t.Function.Arguments = tc.Function.Name, args
				msg.ToolCalls = append(msg.ToolCalls, t)
				pending = append(pending, id)
			}
			if m.Content != "" || len(msg.ToolCalls) == 0 {
				msg.Content = m.Content
			}
			out = append(out, msg)
		case "tool":
			if len(pending) == 0 {
				out = append(out, message{Role: "user", Content: "[Lookup result] " + m.Content})
				continue
			}
			out = append(out, message{Role: "tool", ToolCallID: pending[0], Content: m.Content})
			pending = pending[1:]
		default:
			if len(m.Images) == 0 {
				out = append(out, message{Role: m.Role, Content: m.Content})
				continue
			}
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64," + img}})
			}
			out = append(out, message{Role: m.Role, Content: parts})
		}
	}
	return out
}

// whole reads a reply that came all at once: an answer, or an error sent
// with a success status.
func whole(body []byte) (string, []toolCall, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", nil, errors.New("the provider sent an empty reply")
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Choices) == 0 {
		if msg := errorMessage(body); msg != "" {
			return "", nil, fmt.Errorf("the provider: %s", msg)
		}
		if len(body) > 200 {
			body = append(body[:200:200], "…"...)
		}
		return "", nil, fmt.Errorf("the provider sent a reply this app can't read: %s", body)
	}
	m := r.Choices[0].Message
	return m.Content, m.ToolCalls, nil
}
