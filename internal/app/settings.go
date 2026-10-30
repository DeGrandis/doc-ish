package app

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/ollama"
	"github.com/DeGrandis/mymedical/internal/store"
)

// Settings is what Settings → Assistant edits.
type Settings struct {
	OllamaURL   string `json:"ollama_url"`
	OllamaFound bool   `json:"ollama_found"` // the address came from MYMEDICAL_OLLAMA_URL
	ChatModel   string `json:"chat_model"`
	VisionModel string `json:"vision_model"`
	NumCtx      int    `json:"num_ctx"`
}

const defaultNumCtx = 16384

func (a *App) settings(st *store.Store) Settings {
	s := Settings{
		OllamaURL:   st.Setting("ollama_url"),
		ChatModel:   st.Setting("chat_model"),
		VisionModel: st.Setting("vision_model"),
	}
	if s.OllamaURL == "" && a.cfg.OllamaURL != "" {
		s.OllamaURL, s.OllamaFound = a.cfg.OllamaURL, true
	}
	s.NumCtx, _ = strconv.Atoi(st.Setting("num_ctx"))
	if s.NumCtx <= 0 {
		s.NumCtx = defaultNumCtx
	}
	return s
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		writeJSON(w, a.settings(st))
	})
}

func (a *App) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req Settings
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
	if req.NumCtx < 2048 || req.NumCtx > 262144 {
		req.NumCtx = defaultNumCtx
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		for k, v := range map[string]string{
			"ollama_url":   req.OllamaURL,
			"chat_model":   strings.TrimSpace(req.ChatModel),
			"vision_model": strings.TrimSpace(req.VisionModel),
			"num_ctx":      strconv.Itoa(req.NumCtx),
		} {
			if err := st.SetSetting(k, v); err != nil {
				storeErr(w, err)
				return
			}
		}
		writeJSON(w, a.settings(st))
	})
}

// handleModels lists the models on the configured (or a given) address.
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.URL.Query().Get("url"))
	if addr == "" {
		a.withStore(w, func(st *store.Store, _ *blobs.Store) { addr = a.settings(st).OllamaURL })
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
	models, err := c.Models(r.Context())
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, map[string]any{"models": models, "public": c.Public()})
}
