package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/ollama"
	"github.com/DeGrandis/mymedical/internal/openai"
	"github.com/DeGrandis/mymedical/internal/store"
)

// Settings is what Settings → Assistant edits. The models run on this
// server's Ollama, or, only after the user types remoteConsent, on a
// hosted provider in the OpenAI format (OpenRouter, say): then everything
// the assistant and the reader send leaves this server.
type Settings struct {
	Provider    string `json:"provider"` // ollama or remote
	OllamaURL   string `json:"ollama_url"`
	OllamaFound bool   `json:"ollama_found"` // the address came from MYMEDICAL_OLLAMA_URL
	ChatModel   string `json:"chat_model"`
	VisionModel string `json:"vision_model"`
	NumCtx      int    `json:"num_ctx"`

	RemoteURL         string `json:"remote_url"`
	RemoteKeySet      bool   `json:"remote_key_set"` // the key itself never goes back to the browser
	RemoteModel       string `json:"remote_model"`
	RemoteVisionModel string `json:"remote_vision_model"`
	RemoteConsent     string `json:"remote_consent,omitempty"` // when the user agreed

	remoteKey string
}

// remoteConsent is what the user types to allow a hosted provider, the
// same words as in Proof of Basis.
const remoteConsent = "I accept my data leaking to a provider"

func acceptsRemote(typed string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(typed), " "), remoteConsent)
}

const (
	defaultNumCtx    = 16384
	defaultRemoteURL = "https://openrouter.ai/api/v1"
)

func (a *App) settings(st *store.Store) Settings {
	s := Settings{
		Provider:          st.Setting("provider"),
		OllamaURL:         st.Setting("ollama_url"),
		ChatModel:         st.Setting("chat_model"),
		VisionModel:       st.Setting("vision_model"),
		RemoteURL:         st.Setting("remote_url"),
		RemoteModel:       st.Setting("remote_model"),
		RemoteVisionModel: st.Setting("remote_vision_model"),
		RemoteConsent:     st.Setting("remote_consent"),
		remoteKey:         st.Setting("remote_key"),
	}
	if s.Provider != "remote" || s.RemoteConsent == "" {
		s.Provider = "ollama"
	}
	if s.OllamaURL == "" && a.cfg.OllamaURL != "" {
		s.OllamaURL, s.OllamaFound = a.cfg.OllamaURL, true
	}
	if s.RemoteURL == "" {
		s.RemoteURL = defaultRemoteURL
	}
	s.RemoteKeySet = s.remoteKey != ""
	s.NumCtx, _ = strconv.Atoi(st.Setting("num_ctx"))
	if s.NumCtx <= 0 {
		s.NumCtx = defaultNumCtx
	}
	return s
}

// remote reports whether the hosted provider is in use.
func (s Settings) remote() bool { return s.Provider == "remote" }

// chatModel and visionModel are the models in use, on whichever side.
func (s Settings) chatModel() string {
	if s.remote() {
		return s.RemoteModel
	}
	return s.ChatModel
}

func (s Settings) visionModel() string {
	if s.remote() {
		return s.RemoteVisionModel
	}
	return s.VisionModel
}

// ready says what's missing before the models can be used, or "".
func (s Settings) ready() string {
	if s.remote() {
		if s.RemoteModel == "" {
			return "pick the hosted provider's model in Settings first"
		}
		return ""
	}
	if s.OllamaURL == "" || s.ChatModel == "" {
		return "connect Ollama and pick a model in Settings first"
	}
	return ""
}

// chatter is the model client the settings call for.
func (s Settings) chatter() (ollama.Chatter, error) {
	if s.remote() {
		return openai.New(s.RemoteURL, s.remoteKey)
	}
	return ollama.New(s.OllamaURL)
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		writeJSON(w, a.settings(st))
	})
}

func (a *App) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings
		RemoteKey    string `json:"remote_key"`    // a new key; empty keeps the saved one
		Consent      string `json:"consent"`       // the typed sentence, to switch to the hosted provider
		ForgetRemote bool   `json:"forget_remote"` // back to Ollama, forgetting the key
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.OllamaURL = strings.TrimSpace(req.OllamaURL)
	if req.OllamaURL != "" {
		if _, err := ollama.New(req.OllamaURL); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	req.RemoteURL = strings.TrimSpace(req.RemoteURL)
	if req.RemoteURL != "" {
		if _, err := openai.New(req.RemoteURL, ""); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if req.NumCtx < 2048 || req.NumCtx > 262144 {
		req.NumCtx = defaultNumCtx
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		if req.ForgetRemote {
			for _, k := range []string{"provider", "remote_key", "remote_model", "remote_vision_model", "remote_consent"} {
				st.SetSetting(k, "")
			}
			st.Log(0, "you", "Stopped using the hosted provider and forgot its API key", nil)
			writeJSON(w, a.settings(st))
			return
		}
		cur := a.settings(st)
		if req.Provider == "remote" && cur.RemoteConsent == "" {
			if !acceptsRemote(req.Consent) {
				writeErr(w, 400, "to use a hosted provider, type: "+remoteConsent)
				return
			}
			st.SetSetting("remote_consent", time.Now().UTC().Format(time.RFC3339))
			st.Log(0, "you", "Let the assistant and the file reader send records to a hosted provider", nil)
		}
		if req.Provider != "remote" {
			req.Provider = "ollama"
		}
		if key := strings.TrimSpace(req.RemoteKey); key != "" {
			st.SetSetting("remote_key", key)
			st.Log(0, "you", "Saved the hosted provider's API key (not shown here)", nil)
		}
		for k, v := range map[string]string{
			"provider":            req.Provider,
			"ollama_url":          req.OllamaURL,
			"chat_model":          strings.TrimSpace(req.ChatModel),
			"vision_model":        strings.TrimSpace(req.VisionModel),
			"num_ctx":             strconv.Itoa(req.NumCtx),
			"remote_url":          req.RemoteURL,
			"remote_model":        strings.TrimSpace(req.RemoteModel),
			"remote_vision_model": strings.TrimSpace(req.RemoteVisionModel),
		} {
			if err := st.SetSetting(k, v); err != nil {
				storeErr(w, err)
				return
			}
		}
		a.wakeReader() // files waiting for a model may be readable now
		writeJSON(w, a.settings(st))
	})
}

// handleModels lists the models on Ollama, or on the hosted provider with
// ?provider=remote. The key typed in the form is used, or the saved one.
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var set Settings
	a.withStore(w, func(st *store.Store, _ *blobs.Store) { set = a.settings(st) })
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if q.Get("provider") == "remote" {
		addr := strings.TrimSpace(q.Get("url"))
		if addr == "" {
			addr = set.RemoteURL
		}
		key := strings.TrimSpace(r.Header.Get("X-Remote-Key"))
		if key == "" && strings.TrimRight(addr, "/") == strings.TrimRight(set.RemoteURL, "/") {
			key = set.remoteKey
		}
		c, err := openai.New(addr, key)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		models, err := c.Models(ctx)
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, map[string]any{"models": models, "public": true, "host": c.Host()})
		return
	}

	addr := strings.TrimSpace(q.Get("url"))
	if addr == "" {
		addr = set.OllamaURL
	}
	if addr == "" {
		writeErr(w, 400, "enter the Ollama address first")
		return
	}
	c, err := ollama.New(addr)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	models, err := c.Models(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, map[string]any{"models": models, "public": c.Public()})
}
