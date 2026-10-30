package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/brand"
	"github.com/DeGrandis/mymedical/internal/ollama"
	"github.com/DeGrandis/mymedical/internal/store"
)

// historyLimit is how many earlier messages go with each question.
const historyLimit = 12

func (a *App) handleChats(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		cs, err := st.Chats(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, cs)
	})
}

func (a *App) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		ms, err := st.Messages(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, ms)
	})
}

func (a *App) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		if err := st.DeleteChat(id); err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// handleChat answers one message, streaming newline-delimited JSON:
// {"chat_id":n}, then {"t":"..."} pieces, then {"done":true} or
// {"error":"..."}.
func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req struct {
		ChatID  int64  `json:"chat_id"`
		Message string `json:"message"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeErr(w, 400, "type a message")
		return
	}

	// Gather everything under the lock, then let it go while the model
	// thinks, so a Lock from Settings isn't held up by a long answer.
	var (
		set     Settings
		system  string
		history []store.Message
		failed  bool
	)
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		p, err := st.Person(pid)
		if err != nil {
			storeErr(w, err)
			failed = true
			return
		}
		set = a.settings(st)
		if set.OllamaURL == "" || set.ChatModel == "" {
			writeErr(w, 400, "connect Ollama and pick a model in Settings first")
			failed = true
			return
		}
		if req.ChatID != 0 {
			owner, err := st.ChatOwner(req.ChatID)
			if err != nil || owner != pid {
				writeErr(w, 404, "that chat isn't on this profile")
				failed = true
				return
			}
			history, _ = st.Messages(req.ChatID)
		} else {
			title := req.Message
			if len([]rune(title)) > 60 {
				title = string([]rune(title)[:60]) + "…"
			}
			if req.ChatID, err = st.NewChat(pid, title); err != nil {
				storeErr(w, err)
				failed = true
				return
			}
		}
		if err := st.AddMessage(req.ChatID, "user", req.Message); err != nil {
			storeErr(w, err)
			failed = true
			return
		}
		system = buildSystemPrompt(st, p)
	})
	if failed {
		return
	}

	client, err := ollama.New(set.OllamaURL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msgs := []ollama.Message{{Role: "system", Content: system}}
	if len(history) > historyLimit {
		history = history[len(history)-historyLimit:]
	}
	for _, m := range history {
		msgs = append(msgs, ollama.Message{Role: m.Role, Content: m.Content})
	}
	msgs = append(msgs, ollama.Message{Role: "user", Content: req.Message})

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(v any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]int64{"chat_id": req.ChatID})
	reply, err := client.Chat(r.Context(), ollama.ChatRequest{Model: set.ChatModel, Messages: msgs, NumCtx: set.NumCtx},
		func(t string) { send(map[string]string{"t": t}) })
	if strings.TrimSpace(reply) != "" {
		a.mu.RLock()
		if a.st != nil {
			if err := a.st.AddMessage(req.ChatID, "assistant", reply); err != nil {
				log.Printf("save reply: %v", err)
			}
		}
		a.mu.RUnlock()
	}
	if err != nil && r.Context().Err() == nil {
		send(map[string]string{"error": err.Error()})
		return
	}
	send(map[string]bool{"done": true})
}

// buildSystemPrompt is the rules, then the profile, files and latest
// measurements of the person being viewed.
func buildSystemPrompt(st *store.Store, p store.Person) string {
	var b strings.Builder
	today := time.Now().Format("2006-01-02")
	fmt.Fprintf(&b, "You are the assistant inside %s, a private app on the family's own server that keeps their medical records in one place. You are looking at %s's records. Today is %s.\n\n", brand.Name, p.Name, today)
	b.WriteString(`Rules:
- Answer from the records below. Never invent results, dates, medications or documents. If something isn't in the records, say so.
- You may explain what results and medical terms mean, point out values outside their reference range, say what you notice, what could explain it and what might be worth asking a doctor about. You are not their doctor and this is not a diagnosis. If something could be urgent or serious, say so plainly and suggest contacting a clinician.
- Be concise and concrete: short paragraphs or bullet lists, values with units, dates as YYYY-MM-DD.
- You can see the list of files below, but not their contents yet.

`)
	pr := p.Profile
	b.WriteString("## Profile\n")
	fmt.Fprintf(&b, "Name: %s\n", p.Name)
	if pr.Born != "" {
		if t, err := time.Parse("2006-01-02", pr.Born); err == nil {
			fmt.Fprintf(&b, "Born: %s (age %d)\n", pr.Born, age(t, time.Now()))
		}
	}
	line := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	line("Sex", pr.Sex)
	line("Height", pr.Height)
	line("Blood type", pr.BloodType)
	items := func(k string, list []store.Item) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", k)
		for _, it := range list {
			s := "- " + it.Name
			if it.Detail != "" {
				s += " (" + it.Detail + ")"
			}
			if it.Since != "" {
				s += ", since " + it.Since
			}
			b.WriteString(s + "\n")
		}
	}
	items("Conditions", pr.Conditions)
	items("Surgeries", pr.Surgeries)
	items("Allergies", pr.Allergies)
	items("Medications", pr.Medications)
	items("Family history", pr.FamilyHistory)
	items("Vaccinations", pr.Vaccinations)
	items("Providers", pr.Providers)
	line("Lifestyle", pr.Lifestyle)
	line("Notes", pr.Notes)

	if docs, _ := st.Documents(p.ID, "", 40); len(docs) > 0 {
		b.WriteString("\n## Files (newest first)\n")
		for _, d := range docs {
			title := d.Title
			if title == "" {
				title = d.Filename
			}
			s := "- " + title
			if d.Kind != "" {
				s += " [" + d.Kind + "]"
			}
			if d.DocDate != "" {
				s += ", dated " + d.DocDate
			}
			s += ", added " + d.Added[:10]
			b.WriteString(s + "\n")
		}
	}

	if obs, _ := st.Observations(p.ID, "", 0); len(obs) > 0 {
		latest := map[string]store.Observation{}
		var order []string
		count := map[string]int{}
		for _, o := range obs {
			k := strings.ToLower(o.Name)
			if _, ok := latest[k]; !ok {
				order = append(order, k)
			}
			latest[k] = o
			count[k]++
		}
		b.WriteString("\n## Latest measurements\n")
		for i, k := range order {
			if i >= 40 {
				break
			}
			o := latest[k]
			v := o.ValueText
			if o.Value != nil {
				v = strconv.FormatFloat(*o.Value, 'f', -1, 64)
			}
			s := fmt.Sprintf("- %s: %s %s on %s", o.Name, v, o.Unit, o.Observed)
			if o.RefLow != nil || o.RefHigh != nil {
				s += fmt.Sprintf(" (reference %s–%s)", fnum(o.RefLow), fnum(o.RefHigh))
			}
			if count[k] > 1 {
				s += fmt.Sprintf("; %d readings in all", count[k])
			}
			b.WriteString(strings.TrimSpace(s) + "\n")
		}
	}
	return b.String()
}

func fnum(f *float64) string {
	if f == nil {
		return "?"
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}

func age(born, now time.Time) int {
	y := now.Year() - born.Year()
	if now.YearDay() < born.YearDay() {
		y--
	}
	return y
}
