// Package ollama talks to the user's own Ollama server: which models it
// has and what they can do, and streaming chat. Nothing here reaches
// anything but the address the user gave.
package ollama

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
	"time"
)

type Client struct {
	base string
	http *http.Client
}

// New checks the address and returns a client for it.
func New(base string) (*Client, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("enter the Ollama address like http://192.168.1.20:11434")
	}
	return &Client{base: base, http: &http.Client{}}, nil
}

// Public reports whether the address resolves outside the local network,
// so the UI can warn before medical records are sent there.
func (c *Client) Public() bool {
	u, _ := url.Parse(c.base)
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return true
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Model is one installed model and what it can do.
type Model struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Tools  bool   `json:"tools"`
	Vision bool   `json:"vision"`
	Embed  bool   `json:"embed"`
}

// Models lists the installed models with their capabilities.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := c.do(ctx, "GET", "/api/tags", nil, &tags); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(tags.Models))
	for _, t := range tags.Models {
		m := Model{Name: t.Name, Size: t.Size}
		var show struct {
			Capabilities []string `json:"capabilities"`
		}
		if c.do(ctx, "POST", "/api/show", map[string]string{"model": t.Name}, &show) == nil {
			for _, cap := range show.Capabilities {
				switch cap {
				case "tools":
					m.Tools = true
				case "vision":
					m.Vision = true
				case "embedding":
					m.Embed = true
				}
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Message is one chat message. Images are base64 (for vision models).
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"` // on a tool result
}

// ToolCall is the model asking to run a tool.
type ToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// Tool describes a function the model may call; Parameters is a JSON
// schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

func (t Tool) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":     "function",
		"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters},
	})
}

// ChatRequest is a streaming chat call.
type ChatRequest struct {
	Model    string
	Messages []Message
	NumCtx   int
	Tools    []Tool
}

// ErrNoTools means the model can't use tools.
var ErrNoTools = errors.New("this model can't use tools")

// Chat streams the reply, calling onText with each piece as it arrives,
// and returns the whole reply and any tools the model asked to run.
func (c *Client) Chat(ctx context.Context, r ChatRequest, onText func(string)) (string, []ToolCall, error) {
	body := map[string]any{
		"model":    r.Model,
		"messages": r.Messages,
		"stream":   true,
		"think":    false,
		"options":  map[string]any{"temperature": 0.2, "num_ctx": r.NumCtx},
	}
	if len(r.Tools) > 0 {
		body["tools"] = r.Tools
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		err := apiError(resp)
		if strings.Contains(err.Error(), "does not support tools") {
			return "", nil, ErrNoTools
		}
		return "", nil, err
	}
	var all strings.Builder
	var calls []ToolCall
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var chunk struct {
			Message Message `json:"message"`
			Done    bool    `json:"done"`
			Error   string  `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return all.String(), calls, errors.New(chunk.Error)
		}
		if t := chunk.Message.Content; t != "" {
			all.WriteString(t)
			onText(t)
		}
		calls = append(calls, chunk.Message.ToolCalls...)
		if chunk.Done {
			break
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return all.String(), calls, friendly(err)
	}
	return all.String(), calls, ctx.Err()
}

// ChatJSON asks for one answer shaped by a JSON schema (Ollama's
// structured outputs) and returns it unparsed.
func (c *Client) ChatJSON(ctx context.Context, r ChatRequest, schema any) (string, error) {
	body := map[string]any{
		"model":    r.Model,
		"messages": r.Messages,
		"stream":   false,
		"think":    false,
		"format":   schema,
		"options":  map[string]any{"temperature": 0, "num_ctx": r.NumCtx},
	}
	var out struct {
		Message Message `json:"message"`
	}
	if err := c.do(ctx, "POST", "/api/chat", body, &out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("Ollama: %s", e.Error)
	}
	return fmt.Errorf("Ollama answered %s", resp.Status)
}

func friendly(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("Ollama didn't answer in time")
	}
	if strings.Contains(err.Error(), "connection refused") {
		return errors.New("couldn't reach Ollama at that address; is it running, and listening on the network (OLLAMA_HOST=0.0.0.0)?")
	}
	return fmt.Errorf("couldn't reach Ollama: %w", err)
}
