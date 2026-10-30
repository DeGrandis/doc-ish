package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DeGrandis/mymedical/internal/reference"
)

// refJob is the one reference download that may run at a time.
type refJob struct {
	mu    sync.Mutex
	state refState
}

// refState is what Settings shows of the download.
type refState struct {
	Source  string `json:"source,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Error   string `json:"error,omitempty"`
	Running bool   `json:"running"`
}

func (j *refJob) snapshot() refState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

func (j *refJob) set(f func(*refState)) {
	j.mu.Lock()
	f(&j.state)
	j.mu.Unlock()
}

// refDownloads are the sources the app fetches itself, and roughly how
// much each downloads, for Settings to say before anyone presses it.
var refDownloads = map[string]string{
	reference.MedlinePlus: "about 5 MB",
	reference.RxNorm:      "about 70 MB",
	reference.Labels:      "about 1.8 GB, in a dozen parts; keeps about 100 MB",
}

func (a *App) handleReference(w http.ResponseWriter, r *http.Request) {
	if a.ref == nil {
		writeErr(w, 500, "the reference database isn't open")
		return
	}
	writeJSON(w, map[string]any{"sources": a.ref.Sources(), "job": a.job.snapshot(), "downloads": refDownloads})
}

// handleReferenceDownload starts fetching a source in the background.
// This is the only time the app contacts NLM or the FDA.
func (a *App) handleReferenceDownload(w http.ResponseWriter, r *http.Request) {
	src := r.PathValue("source")
	if _, ok := refDownloads[src]; !ok || a.ref == nil {
		writeErr(w, 404, "no such source")
		return
	}
	started := false
	a.job.set(func(j *refState) {
		if !j.Running {
			*j = refState{Source: src, Stage: "Starting", Running: true}
			started = true
		}
	})
	if !started {
		writeErr(w, 409, "another download is running; wait for it to finish")
		return
	}
	go a.runRefJob(src)
	writeJSON(w, a.job.snapshot())
}

func (a *App) runRefJob(src string) {
	progress := func(stage string, done, total int64) {
		a.job.set(func(j *refState) { j.Stage, j.Done, j.Total = stage, done, total })
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Hour)
	defer cancel()
	client := &http.Client{}
	tmp := filepath.Join(a.cfg.DataDir, "tmp")
	os.MkdirAll(tmp, 0o700)
	var err error
	switch src {
	case reference.MedlinePlus:
		progress("Downloading MedlinePlus topics", 0, 0)
		var version string
		var topics []reference.Topic
		if version, topics, err = reference.FetchTopics(ctx, client); err == nil {
			err = a.ref.ImportTopics(version, topics)
		}
	case reference.RxNorm:
		err = a.ref.FetchRxNorm(ctx, client, tmp, progress)
	case reference.Labels:
		err = a.ref.FetchLabels(ctx, client, tmp, progress)
	}
	a.job.set(func(j *refState) {
		j.Running = false
		j.Stage = "Done"
		if err != nil {
			j.Error = err.Error()
			j.Stage = "Failed"
		}
	})
	if err != nil {
		log.Printf("reference %s: %v", src, err)
	}
}

// handleLOINC imports the LOINC release zip someone downloaded from
// loinc.org with their own account.
func (a *App) handleLOINC(w http.ResponseWriter, r *http.Request) {
	if a.ref == nil {
		writeErr(w, 500, "the reference database isn't open")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "send the zip as a form upload")
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		writeErr(w, 400, "no file in the upload")
		return
	}
	tmp := filepath.Join(a.cfg.DataDir, "tmp")
	os.MkdirAll(tmp, 0o700)
	f, err := os.CreateTemp(tmp, "loinc-*.zip")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, io.LimitReader(part, 2<<30))
	f.Close()
	if err != nil {
		writeErr(w, 400, "upload interrupted")
		return
	}
	n, err := a.ref.ImportLOINC(f.Name(), part.FileName())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"tests": n, "sources": a.ref.Sources()})
}

// ---------- the assistant's lookups ----------

const topicLimit = 3000

func (e *toolEnv) explain(q string) (string, error) {
	if e.ref == nil {
		return "", errors.New("the medical reference isn't available")
	}
	topics, err := e.ref.Explain(q, 2)
	if err != nil {
		return "", err
	}
	if len(topics) == 0 {
		e.note("explain", "MedlinePlus: nothing on "+q, 0)
		return fmt.Sprintf("MedlinePlus has no topic on %q. Say so rather than explaining from memory, or try other words.", q), nil
	}
	var out string
	for _, t := range topics {
		e.noteLink("explain", "MedlinePlus: "+t.Title, t.URL)
		s := t.Summary
		if len(s) > topicLimit {
			s = s[:topicLimit] + " […]"
		}
		out += fmt.Sprintf("## %s (%s)\n%s\n\n", t.Title, t.URL, s)
	}
	return out + reference.Credits[reference.MedlinePlus] + " Base your explanation on this and mention MedlinePlus as the source.", nil
}

func (e *toolEnv) lookupDrug(q string) (string, error) {
	if e.ref == nil || (!e.ref.Has(reference.RxNorm) && !e.ref.Has(reference.Labels)) {
		return "The drug reference (RxNorm and FDA labels) isn't downloaded; they can be in Settings → Reference data. Say you can't look the drug up rather than answering from memory.", nil
	}
	d, err := e.ref.LookupDrug(q)
	if err != nil {
		return "", err
	}
	e.note("lookup_drug", "Drug reference: "+q, 0)
	if d == nil {
		return fmt.Sprintf("No drug called %q in the reference. Check the spelling, or say you couldn't find it.", q), nil
	}
	var b []string
	if d.IsBrand {
		b = append(b, fmt.Sprintf("%s is a brand name.", d.Name))
	}
	if len(d.Ingredients) > 0 {
		b = append(b, "Active ingredient(s): "+join(d.Ingredients))
	}
	if len(d.Brands) > 0 && !d.IsBrand {
		b = append(b, "Brand names: "+join(d.Brands))
	}
	if len(d.Products) > 0 {
		b = append(b, "Products and strengths:\n- "+joinLines(d.Products))
	}
	if l := d.Label; l != nil {
		b = append(b, fmt.Sprintf("FDA label for %s (effective %s):", l.Generic, l.Effective))
		add := func(name, text string, limit int) {
			if text != "" {
				if len(text) > limit {
					text = text[:limit] + " […]"
				}
				b = append(b, name+": "+text)
			}
		}
		add("Boxed warning", l.Boxed, 1500)
		add("What it's for", l.Indications, 1200)
		add("Don't use when", l.Contraindications, 1000)
		add("Warnings", l.Warnings, 1500)
		if l.Interactions != "" {
			b = append(b, "The label has a drug interactions section; use drug_interactions to check it against other drugs.")
		}
		b = append(b, reference.Credits[reference.Labels])
	} else if !e.ref.Has(reference.Labels) {
		b = append(b, "(FDA labels aren't downloaded, so no warnings or uses are shown.)")
	}
	if e.ref.Has(reference.RxNorm) {
		b = append(b, reference.Credits[reference.RxNorm])
	}
	return joinParas(b), nil
}

func (e *toolEnv) drugInteractions(drugs []string) (string, error) {
	if e.ref == nil || !e.ref.Has(reference.Labels) {
		return "FDA drug labels aren't downloaded, so interactions can't be checked; they can be in Settings → Reference data. Say so, and suggest asking a pharmacist.", nil
	}
	if len(drugs) < 2 {
		return "", errors.New("name at least two drugs")
	}
	found, noLabel, err := e.ref.Interactions(drugs)
	if err != nil {
		return "", err
	}
	e.note("drug_interactions", "Interactions: "+join(drugs), 0)
	var b []string
	for _, m := range found {
		b = append(b, fmt.Sprintf("The %s label mentions %s:\n- %s", m.Label, m.Mentions, joinLines(m.Passages)))
	}
	if len(found) == 0 {
		b = append(b, "None of these labels names another of these drugs. That is not proof they're safe together: labels often speak of drug classes (\"statins\", \"CYP3A4 inhibitors\") instead of names.")
	}
	if len(noLabel) > 0 {
		b = append(b, "No FDA label found for: "+join(noLabel)+".")
	}
	b = append(b, reference.Credits[reference.Labels]+" Quote what the labels say; suggest a pharmacist or doctor for anything that matters.")
	return joinParas(b), nil
}

func (e *toolEnv) labTest(q string) (string, error) {
	var b []string
	if e.ref != nil && e.ref.Has(reference.LOINC) {
		tests, err := e.ref.LabTests(q, 5)
		if err != nil {
			return "", err
		}
		for _, t := range tests {
			s := fmt.Sprintf("- LOINC %s: %s", t.Code, t.Name)
			if t.Units != "" {
				s += " (units " + t.Units + ")"
			}
			b = append(b, s)
		}
		if len(b) > 0 {
			b = append([]string{"Standard lab test names (LOINC):"}, b...)
			b = append(b, "LOINC content © Regenstrief Institute, Inc.")
		}
	}
	more, err := e.explain(q)
	if err != nil {
		return "", err
	}
	e.note("lab_test", "Lab test: "+q, 0)
	return joinParas(append(b, more)), nil
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n- "
		}
		out += s
	}
	return out
}

func joinParas(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n\n"
		}
		out += s
	}
	return out
}
