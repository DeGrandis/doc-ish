package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DeGrandis/doc-ish/internal/blobs"
	"github.com/DeGrandis/doc-ish/internal/brand"
	"github.com/DeGrandis/doc-ish/internal/ollama"
	"github.com/DeGrandis/doc-ish/internal/store"
)

// historyLimit is how many earlier messages go with each question.
const historyLimit = 12

// contextRoom is how many tokens of the model's context one question has
// to fill: what's left after the tools, the longest answer and a margin
// for the estimate being off. Past the context Ollama silently drops the
// start of the prompt, which is the rules and the tools, so nothing sent
// may be allowed to grow with the records.
func contextRoom(numCtx int) int {
	return max(numCtx*9/10-ollama.MaxAnswerTokens-toolTokens(), 1500)
}

func toolTokens() int {
	b, _ := json.Marshal(assistantTools)
	return ollama.Tokens(string(b))
}

// within writes lines until the tokens run out and returns how many it
// left out.
func within(b *strings.Builder, lines []string, tokens int) int {
	for i, l := range lines {
		if tokens -= ollama.Tokens(l); tokens < 0 {
			return len(lines) - i
		}
		b.WriteString(l + "\n")
	}
	return 0
}

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
		room := contextRoom(set.NumCtx)
		system = buildSystemPrompt(st, p, room) + viewingSection(st, pid, req.Viewing, room)
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
	for left, i := contextRoom(set.NumCtx)/6, len(history)-1; i >= 0; i-- {
		if left -= ollama.Tokens(history[i].Content); left < 0 {
			history = history[i+1:]
			break
		}
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

// savingTools are the tools that change the records.
var savingTools = map[string]bool{"note_value": true, "update_profile": true, "add_follow_up": true, "set_follow_up": true}

var savedWord = regexp.MustCompile(`(?i)\b(saved|noted|logged|recorded|added|updated)\b`)

// claimsSaved reports an answer that looks like the short confirmation of
// a save, to something the user told it rather than asked.
func claimsSaved(said, answer string) bool {
	return !strings.Contains(said, "?") && len(answer) < 300 && savedWord.MatchString(answer)
}

// converse runs the tool loop: the model answers or asks for tools; tools
// run against the records and their results go back, until it answers.
// Text streams to the browser as it comes; so do charts, opened files and
// changes, as events.
func (a *App) converse(ctx context.Context, client ollama.Chatter, set Settings, pid int64, msgs []ollama.Message,
	extra *msgExtra, send func(any)) (string, error) {
	tools := assistantTools
	var reply strings.Builder
	done := map[string]bool{} // tool calls already run, by name and arguments
	room := contextRoom(set.NumCtx)
	keep := min(ollama.Tokens(msgs[0].Content)+toolTokens(), set.NumCtx/2)
	said := msgs[len(msgs)-1].Content
	saved, reminded, pressed := false, false, false
	for round := 0; ; round++ {
		if round == maxRounds {
			tools = nil // make it answer with what it has
		}
		fit(msgs, room)
		text, calls, err := client.Chat(ctx, ollama.ChatRequest{Model: set.chatModel(), Messages: msgs, NumCtx: set.NumCtx, Tools: tools, Keep: keep, Think: true},
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
			if strings.TrimSpace(reply.String()) == "" {
				// A reasoning model can think a tool's result through and
				// stop there, which would leave the chat blank.
				if !pressed {
					pressed = true
					msgs = append(msgs, ollama.Message{Role: "user", Content: "You haven't answered yet. Answer now, from what the tools returned."})
					continue
				}
				const noAnswer = "_No answer came back. Please ask again._"
				reply.WriteString(noAnswer)
				send(map[string]string{"t": noAnswer})
				return reply.String(), nil
			}
			if tools == nil || saved || !claimsSaved(said, text) {
				return reply.String(), nil
			}
			// Models sometimes say "saved" without calling the tool, and
			// nothing on the screen would show that it wasn't.
			reply.WriteString("\n\n")
			send(map[string]string{"t": "\n\n"})
			if reminded {
				const notSaved = "_Nothing was saved. Please say it again._"
				reply.WriteString(notSaved)
				send(map[string]string{"t": notSaved})
				return reply.String(), nil
			}
			reminded = true
			msgs = append(msgs, ollama.Message{Role: "assistant", Content: text}, ollama.Message{Role: "user",
				Content: "No tool ran, so nothing was saved. If there's something to save, call the tool now. If not, say so."})
			continue
		}
		if strings.TrimSpace(text) != "" {
			reply.WriteString("\n\n")
			send(map[string]string{"t": "\n\n"})
		}
		msgs = append(msgs, ollama.Message{Role: "assistant", Content: text, ToolCalls: calls})
		for _, c := range calls {
			var result string
			key := c.Function.Name + string(c.Function.Arguments)
			if done[key] {
				// Small models sometimes repeat a lookup that came back
				// unhelpful; running it again changes nothing.
				msgs = append(msgs, ollama.Message{Role: "tool", ToolName: c.Function.Name,
					Content: "You already did exactly this above and have its result. Don't repeat it: answer with what you have, and say what you couldn't find."})
				continue
			}
			done[key] = true
			saved = saved || savingTools[c.Function.Name]
			a.mu.RLock()
			if a.st == nil {
				result = "Error: the records were locked."
			} else {
				env := &toolEnv{st: a.st, files: a.files, set: set, ref: a.ref, pid: pid, today: time.Now(), emit: send, extra: extra}
				result = env.run(c)
			}
			a.mu.RUnlock()
			if short, cut := ollama.Clip(result, room/3); cut {
				result = short + "\n[…cut to fit. Ask for less: one name, or a date range]"
			}
			msgs = append(msgs, ollama.Message{Role: "tool", ToolName: c.Function.Name, Content: result})
		}
	}
}

// fit makes room for another round by dropping the oldest tool results.
func fit(msgs []ollama.Message, room int) {
	total := 0
	for _, m := range msgs {
		total += ollama.Tokens(m.Content)
	}
	for i := range msgs[:len(msgs)-1] {
		if total <= room {
			return
		}
		if msgs[i].Role == "tool" {
			total -= ollama.Tokens(msgs[i].Content)
			msgs[i].Content = "[Dropped to make room. Answer with what you have.]"
		}
	}
}

// viewingSection tells the model about the file open beside the chat, so
// "this" and "here" mean it.
func viewingSection(st *store.Store, pid, docID int64, room int) string {
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
		if n := within(&b, fileValues(vals), room/12); n > 0 {
			fmt.Fprintf(&b, "- …and %d more values; get_values finds any by name\n", n)
		}
	}
	if t := strings.TrimSpace(d.Text); t != "" {
		short, cut := ollama.Clip(strings.ToValidUTF8(t[:min(len(t), 3000)], ""), room/12)
		if cut || len(t) > 3000 {
			short += "\n[…more with read_file]"
		}
		b.WriteString("Its text:\n" + short + "\n")
	}
	return b.String()
}

// buildSystemPrompt is the rules, then the profile, files and latest
// measurements of the person being viewed.
func buildSystemPrompt(st *store.Store, p store.Person, room int) string {
	var b strings.Builder
	today := time.Now().Format("2006-01-02")
	fmt.Fprintf(&b, "You are the assistant inside %s, a private app on the family's own server that keeps their medical records in one place. You are looking at %s's records. Today is %s.\n\n", brand.Name, p.Name, today)
	b.WriteString(`How to answer:
- Put the answer in the first sentence. No greeting, no repeating the question, no "Let me look", no summing up at the end.
- One to three sentences, or a short list when there are several values. Go longer only when asked, or when something could be urgent or serious, which you say plainly, suggesting they contact a clinician.
- Only what was asked: no general health advice they didn't ask for.
- Values with units, dates as YYYY-MM-DD. Name files by title and date, never by an id like [doc:12].
- You're not their doctor and this isn't a diagnosis, but do explain what results mean, point out what's out of range and say what could explain it.

Finding things:
- Answer only from the records. Never invent results, dates, medications or files. If something isn't there, say so.
- Below is the profile, the newest files and the latest measurements. That's only part of the records: if something isn't below, look with your tools before saying it isn't there. read_file for what a file says, get_values for every reading of something, search_files to find files.
- Trends and graphs: call chart, then describe the trend in a sentence or two. To see or open a file: call open_file.
- What a condition, symptom, test or term means: call explain and base the answer on it, mentioning MedlinePlus. A medication: lookup_drug. Whether medications go together, or side effects of a combination: drug_interactions with every medication involved, quoting the FDA labels. If the reference has nothing, say so.
- Imaging: describe_image has the vision model look at the study. Give what it says as what an AI notices, not a radiologist's reading; a report is what counts. For what a report or any other file says, use read_file.

Saving things:
- A measurement ("weight 182 today", "BP 128/82 this morning"): save it with note_value at once and confirm in one short line. An allergy, condition, medication, surgery, family history, vaccination, doctor, birth date, sex, height or blood type: save it with update_profile. Don't ask first: every change shows an Undo button. Several things at once: one tool call each.
- A reminder, appointment or test to come: add_follow_up. One that's done or no longer needed: set_follow_up. "Coming up" lists what's due.
- Only say you saved something after the tool answered "Saved". No tool call means nothing was saved.
- To fill in their profile, ask about one or two empty sections at a time like a friendly intake nurse, save each answer, and let them skip anything.

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

	secs := map[int64][]store.Section{}
	if all, err := st.PersonSections(p.ID); err == nil {
		for _, s := range all {
			secs[s.DocumentID] = append(secs[s.DocumentID], s)
		}
	}
	if docs, _ := st.Documents(p.ID, "", 200); len(docs) > 0 {
		b.WriteString("\n## Files (newest first; the [doc:N] id is for your tools)\n")
		var lines []string
		for _, d := range docs {
			var l strings.Builder
			l.WriteString("- " + docLine(d))
			// A records release lists the visits and reports in it, so
			// "my visit in September" finds it.
			n := 0
			for _, s := range secs[d.ID] {
				if s.Date == "" {
					continue
				}
				if n++; n > 15 {
					l.WriteString("\n  - …and more; read_file lists them all")
					break
				}
				fmt.Fprintf(&l, "\n  - %s %s: %s (pages %d–%d)", s.Date, s.Kind, s.Title, s.FirstPage, s.LastPage)
			}
			lines = append(lines, l.String())
		}
		if within(&b, lines, room/8) > 0 {
			b.WriteString("- …and older files; search_files finds them\n")
		}
	}

	if fs, _ := st.FollowUps(p.ID); len(fs) > 0 {
		var open []string
		for _, f := range fs {
			if f.Status == store.FollowUpOpen {
				open = append(open, "- "+followLine(f, today))
			}
		}
		if len(open) > 0 {
			b.WriteString("\n## Coming up (follow-ups from files or the user; [f:N] is for set_follow_up)\n")
			if n := within(&b, open, room/20); n > 0 {
				fmt.Fprintf(&b, "- …and %d more\n", n)
			}
		}
	}

	if ms, _ := st.Measurements(p.ID); len(ms) > 0 {
		b.WriteString("\n## Latest measurements\n")
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].Latest.Observed > ms[j].Latest.Observed })
		var lines []string
		for _, m := range ms {
			o := m.Latest
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
			if strings.HasPrefix(o.Note, "daily") || strings.HasPrefix(o.Note, "the day's") {
				s += " (" + o.Note + ")"
			}
			// Where it came from, so "my hospital records" or "my watch"
			// finds it, and the file can be read.
			if strings.HasPrefix(o.Note, "from hospital records") {
				s += " (from hospital records)"
			}
			if o.DocumentID != nil {
				s += fmt.Sprintf(" [doc:%d]", *o.DocumentID)
			}
			if m.Count > 1 {
				s += fmt.Sprintf("; %d readings since %s", m.Count, m.First[:min(len(m.First), 10)])
			}
			lines = append(lines, strings.TrimSpace(s))
		}
		if n := within(&b, lines, room/10); n > 0 {
			fmt.Fprintf(&b, "- …and %d more; list_measurements has them all\n", n)
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
