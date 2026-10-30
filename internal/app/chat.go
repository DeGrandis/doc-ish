package app

import (
	"context"
	"encoding/json"
	"errors"
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
		Viewing int64  `json:"viewing"` // the file open beside the chat
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
		if msg := set.ready(); msg != "" {
			writeErr(w, 400, msg)
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
		if err := st.AddMessage(req.ChatID, "user", req.Message, nil); err != nil {
			storeErr(w, err)
			failed = true
			return
		}
		system = buildSystemPrompt(st, p) + viewingSection(st, pid, req.Viewing)
	})
	if failed {
		return
	}

	client, err := set.chatter()
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
	send(map[string]any{"chat_id": req.ChatID, "build": a.build})

	extra := &msgExtra{}
	reply, err := a.converse(r.Context(), client, set, pid, msgs, extra, send)
	reply = strings.TrimSpace(reply) // some providers pad answers with blank lines
	if strings.TrimSpace(reply) != "" || len(extra.Tools) > 0 {
		a.mu.RLock()
		if a.st != nil {
			if err := a.st.AddMessage(req.ChatID, "assistant", reply, extra); err != nil {
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

// maxRounds is how many times the model may call tools for one question.
const maxRounds = 6

// converse runs the tool loop: the model answers or asks for tools; tools
// run against the records and their results go back, until it answers.
// Text streams to the browser as it comes; so do charts, opened files and
// changes, as events.
func (a *App) converse(ctx context.Context, client ollama.Chatter, set Settings, pid int64, msgs []ollama.Message,
	extra *msgExtra, send func(any)) (string, error) {
	tools := assistantTools
	var reply strings.Builder
	for round := 0; ; round++ {
		if round == maxRounds {
			tools = nil // make it answer with what it has
		}
		text, calls, err := client.Chat(ctx, ollama.ChatRequest{Model: set.chatModel(), Messages: msgs, NumCtx: set.NumCtx, Tools: tools},
			func(t string) {
				reply.WriteString(t)
				send(map[string]string{"t": t})
			})
		if errors.Is(err, ollama.ErrNoTools) && tools != nil {
			send(map[string]string{"t": "_This model can't use tools, so I can only answer from the summary. Pick a model with tool use in Settings._\n\n"})
			tools = nil
			round = maxRounds
			continue
		}
		if err != nil {
			return reply.String(), err
		}
		if len(calls) == 0 || tools == nil {
			return reply.String(), nil
		}
		if strings.TrimSpace(text) != "" {
			reply.WriteString("\n\n")
			send(map[string]string{"t": "\n\n"})
		}
		msgs = append(msgs, ollama.Message{Role: "assistant", Content: text, ToolCalls: calls})
		for _, c := range calls {
			var result string
			a.mu.RLock()
			if a.st == nil {
				result = "Error: the records were locked."
			} else {
				env := &toolEnv{st: a.st, ref: a.ref, pid: pid, today: time.Now(), emit: send, extra: extra}
				result = env.run(c)
			}
			a.mu.RUnlock()
			msgs = append(msgs, ollama.Message{Role: "tool", ToolName: c.Function.Name, Content: result})
		}
	}
}

// viewingSection tells the model about the file open beside the chat, so
// "this" and "here" mean it.
func viewingSection(st *store.Store, pid, docID int64) string {
	if docID == 0 {
		return ""
	}
	d, err := st.Document(docID)
	if err != nil || d.PersonID != pid {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## What the user is looking at\nThis file is open beside the chat; \"this\", \"it\" and \"here\" mean it unless they say otherwise.\n")
	b.WriteString(docLine(d) + "\n")
	if vals, _ := st.DocumentObservations(docID); len(vals) > 0 {
		for _, o := range vals {
			b.WriteString("- " + obsLine(o) + "\n")
		}
	}
	if t := strings.TrimSpace(d.Text); t != "" {
		if len(t) > 3000 {
			t = strings.ToValidUTF8(t[:3000], "") + "\n[…more with read_file]"
		}
		b.WriteString("Its text:\n" + t + "\n")
	}
	return b.String()
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
- Below is the profile, the files with a short summary of each, and the latest value of every measurement. For anything more, use your tools and answer from what they return: read_file for what a file says, get_values for every reading of something over time, search_files to find files.
- Before explaining what a condition, symptom, test or term means, call explain (MedlinePlus) and base the explanation on what it returns, mentioning MedlinePlus. For a medication, call lookup_drug. When asked whether medications go together, or about side effects of a combination, call drug_interactions with every medication involved, and quote what the FDA labels say. If the reference has nothing, say so instead of filling in from memory.
- When the user asks how something is trending, or for a graph or chart, call chart, then describe the trend in a sentence or two.
- When the user asks to see, pull up or open a file, call open_file.
- When the user tells you a measurement ("weight 182 today", "BP 128/82 this morning"), save it with note_value right away, then confirm in one short line. When they tell you about an allergy, condition, medication, surgery, family history, vaccination or doctor, or their birth date, sex, height or blood type, save it with update_profile. Every change shows to them with an Undo button, so don't ask "shall I save it?" first. When they give several things at once (two family members, three medications), call the tool once for each.
- Only say you saved, added or updated something after the tool answered "Saved". If you didn't call a tool, nothing was saved; never claim otherwise.
- If the user asks you to help fill in their profile, interview them like a friendly intake nurse: look at which profile sections are empty, ask about one or two at a time, save each answer as you get it, and move on. Let them skip anything.
- Don't paste ids like [doc:12] into answers; name files by title and date.
- Don't narrate that you're about to look something up; just call the tool.

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
	line("Emergency contact", pr.EmergencyContact)
	line("Insurance", pr.Insurance)
	line("Notes", pr.Notes)
	var empty []string
	for _, f := range []struct {
		name  string
		blank bool
	}{
		{"born", pr.Born == ""}, {"sex", pr.Sex == ""}, {"height", pr.Height == ""}, {"blood type", pr.BloodType == ""},
		{"allergies", len(pr.Allergies) == 0}, {"medications", len(pr.Medications) == 0}, {"conditions", len(pr.Conditions) == 0},
		{"surgeries", len(pr.Surgeries) == 0}, {"family history", len(pr.FamilyHistory) == 0}, {"vaccinations", len(pr.Vaccinations) == 0},
		{"doctors", len(pr.Providers) == 0}, {"lifestyle", pr.Lifestyle == ""}, {"emergency contact", pr.EmergencyContact == ""},
	} {
		if f.blank {
			empty = append(empty, f.name)
		}
	}
	if len(empty) > 0 {
		b.WriteString("Not filled in yet: " + strings.Join(empty, ", ") + "\n")
	}

	if docs, _ := st.Documents(p.ID, "", 40); len(docs) > 0 {
		b.WriteString("\n## Files (newest first; the [doc:N] id is for your tools)\n")
		for _, d := range docs {
			b.WriteString("- " + docLine(d) + "\n")
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
			if o.OutOfRange() {
				s += " OUT OF RANGE"
				if o.Flag != "" {
					s += " (flag " + o.Flag + ")"
				}
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
