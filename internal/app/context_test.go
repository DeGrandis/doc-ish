package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/DeGrandis/doc-ish/internal/ollama"
	"github.com/DeGrandis/doc-ish/internal/store"
)

// seedHuge fills a profile with far more than fits: 60 files, the last a
// long release with hundreds of values and records.
func seedHuge(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var long int64
	err := st.Tx(func(tx *sql.Tx) error {
		for i := range 60 {
			id, err := store.AddDocumentTx(tx, store.Document{PersonID: 1, Filename: fmt.Sprintf("f%d.pdf", i), SHA256: fmt.Sprint(i), Blob: fmt.Sprint(i),
				Kind: "lab", Title: fmt.Sprintf("Lab panel %d", i), DocDate: fmt.Sprintf("20%02d-03-04", i%20+5), Summary: strings.Repeat("Routine bloodwork. ", 8), Status: store.StatusDone})
			if err != nil {
				return err
			}
			long = id
		}
		secs := []store.Section{}
		for i := range 120 {
			secs = append(secs, store.Section{DocumentID: long, PersonID: 1, Index: i, FirstPage: i + 1, LastPage: i + 1, Kind: "visit", Title: fmt.Sprintf("Office visit %d", i), Date: "2020-02-03"})
		}
		if _, err := store.AddSectionsTx(tx, secs); err != nil {
			return err
		}
		obs := []store.Observation{}
		for i := range 1500 {
			v, low, high := float64(i%300), 60.0, 130.0
			obs = append(obs, store.Observation{PersonID: 1, Name: fmt.Sprintf("Test %d", i%90), Value: &v, Unit: "mg/dL", RefLow: &low, RefHigh: &high,
				Observed: fmt.Sprintf("20%02d-%02d-%02d", i%14+10, i%12+1, i%27+1), DocumentID: &long, Source: "document"})
		}
		if err := store.AddObservationsTx(tx, obs); err != nil {
			return err
		}
		for i := range 80 {
			if _, err := store.AddFollowUpTx(tx, store.FollowUp{PersonID: 1, What: fmt.Sprintf("Repeat test %d", i), Due: "2030-01-01", Source: "document"}); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE documents SET text = ? WHERE id = ?`, strings.Repeat("2020-02-03 | Ferritin | 18.4 | ng/mL | 12-150\n", 2000), long)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return long
}

// However much is on record, every round has to fit the model's context,
// or Ollama drops the rules and the tools from the start of the prompt.
func TestChatFitsTheContext(t *testing.T) {
	o := &scriptedOllama{}
	srv, c, a := signedInApp(t, o.server(t).URL)
	long := seedHuge(t, a.st)
	call := func(name, args string) string {
		return `{"tool_calls":[{"function":{"name":"` + name + `","arguments":` + args + `}}]}`
	}
	o.steps = []string{
		call("read_file", fmt.Sprintf(`{"id":%d}`, long)),
		call("list_measurements", `{}`),
		call("get_values", `{"name":"Test"}`),
		`{"content":"Done."}`,
	}
	chat(t, c, srv.URL+"/api/people/1/chat", "Is anything in this file worth asking about?", long)
	if len(o.seen) != 4 {
		t.Fatalf("%d rounds", len(o.seen))
	}
	for i, body := range o.seen {
		var req struct {
			Messages []ollama.Message `json:"messages"`
			Tools    json.RawMessage  `json:"tools"`
			Options  struct {
				NumCtx  int `json:"num_ctx"`
				NumKeep int `json:"num_keep"`
			} `json:"options"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		used := ollama.Tokens(string(req.Tools)) + ollama.MaxAnswerTokens
		for _, m := range req.Messages {
			used += ollama.Tokens(m.Content)
		}
		if used > req.Options.NumCtx {
			t.Errorf("round %d takes %d tokens of %d", i+1, used, req.Options.NumCtx)
		}
		if req.Options.NumKeep < ollama.Tokens(req.Messages[0].Content) {
			t.Errorf("round %d keeps %d tokens, less than the rules", i+1, req.Options.NumKeep)
		}
	}
	// What's left out is said to be left out, so the model goes looking.
	for _, want := range []string{"older files; search_files", "more; list_measurements", "more values; get_values"} {
		if !strings.Contains(o.seen[0], want) {
			t.Errorf("the prompt doesn't say %q", want)
		}
	}
}

func TestSearchFilesFindsPhrases(t *testing.T) {
	_, _, a := signedInApp(t, (&scriptedOllama{}).server(t).URL)
	var knee int64
	err := a.st.Tx(func(tx *sql.Tx) error {
		var err error
		if knee, err = store.AddDocumentTx(tx, store.Document{PersonID: 1, Filename: "a.pdf", SHA256: "a", Blob: "a", Kind: "imaging", Title: "MRI left knee", DocDate: "2019-05-14", Status: store.StatusDone}); err != nil {
			return err
		}
		_, err = store.AddDocumentTx(tx, store.Document{PersonID: 1, Filename: "b.pdf", SHA256: "b", Blob: "b", Kind: "lab", Title: "Lipid panel", DocDate: "2019-06-01", Status: store.StatusDone})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &toolEnv{st: a.st, pid: 1, set: Settings{NumCtx: 16384}, emit: func(any) {}, extra: &msgExtra{}}
	got, err := env.searchFiles("knee MRI 2019", "", "", "")
	if err != nil || !strings.Contains(got, fmt.Sprintf("[doc:%d]", knee)) || strings.Contains(got, "Lipid") || strings.Contains(got, "closest") {
		t.Fatalf("every word: %q, %v", got, err)
	}
	// A word that's nowhere still finds the file with the others.
	got, err = env.searchFiles("knee MRI report", "", "", "")
	if err != nil || !strings.Contains(got, "The closest:") || !strings.Contains(got, "MRI left knee") {
		t.Fatalf("closest: %q, %v", got, err)
	}
}

func TestLongHistoryOfValuesGoesByYear(t *testing.T) {
	var obs []store.Observation
	for y := 2015; y < 2020; y++ {
		for m := 1; m <= 12; m++ {
			v := float64(m)
			obs = append(obs, store.Observation{Value: &v, Observed: fmt.Sprintf("%d-%02d-05", y, m)})
		}
	}
	s := valueStats(obs)
	if !strings.Contains(s, "By year:") || !strings.Contains(s, "- 2019: average 6.5") || strings.Contains(s, "2019-03") {
		t.Fatal(s)
	}
}
