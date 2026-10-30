package reference

import (
	"archive/zip"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const topicsXML = `<?xml version="1.0" encoding="UTF-8"?>
<health-topics total="3">
<health-topic title="A1C" url="https://medlineplus.gov/a1c.html" id="6308" language="English">
<also-called>HbA1C</also-called>
<full-summary>&lt;p&gt;A1C is a blood test for &lt;a href="x"&gt;type 2 diabetes&lt;/a&gt;.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;A normal A1C level is below 5.7%&lt;/li&gt;&lt;/ul&gt;</full-summary>
<group url="g" id="25">Diagnostic Tests</group>
<see-reference>Hemoglobin A1c</see-reference>
</health-topic>
<health-topic title="Prueba A1C" url="https://medlineplus.gov/spanish/a1c.html" id="6309" language="Spanish">
<full-summary>&lt;p&gt;Spanish&lt;/p&gt;</full-summary>
</health-topic>
<health-topic title="Cholesterol" url="https://medlineplus.gov/cholesterol.html" id="1" language="English">
<also-called>High cholesterol</also-called>
<full-summary>&lt;p&gt;Cholesterol is a waxy, fat-like substance. LDL is called bad cholesterol.&lt;/p&gt;</full-summary>
</health-topic>
</health-topics>`

func openTest(t *testing.T) *Ref {
	t.Helper()
	r, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	topics, err := ParseTopics(strings.NewReader(topicsXML))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ImportTopics("2099-01-01", topics); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTopics(t *testing.T) {
	r := openTest(t)
	if src := r.Sources(); src[0].Items != 2 {
		t.Fatalf("sources: %+v", src)
	}
	got, err := r.Explain("hemoglobin a1c", 2)
	if err != nil || len(got) == 0 || got[0].Title != "A1C" {
		t.Fatalf("explain: %+v %v", got, err)
	}
	if !strings.Contains(got[0].Summary, "- A normal A1C level is below 5.7%") || strings.Contains(got[0].Summary, "<") {
		t.Fatalf("summary: %q", got[0].Summary)
	}
	if got, _ := r.Explain("what is LDL", 2); len(got) == 0 || got[0].Title != "Cholesterol" {
		t.Fatalf("full text: %+v", got)
	}
}

// Lookups go on while an import holds a write transaction open.
func TestLookupDuringImport(t *testing.T) {
	r := openTest(t)
	release := make(chan struct{})
	started := make(chan struct{})
	go r.tx(func(tx *sql.Tx) error {
		tx.Exec(`DELETE FROM topics`)
		close(started)
		<-release
		return errors.New("rolled back")
	})
	<-started
	done := make(chan []Topic)
	go func() { got, _ := r.Explain("A1C", 1); done <- got }()
	select {
	case got := <-done:
		if len(got) != 1 {
			t.Fatalf("explain during import: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lookup waited for the import")
	}
	close(release)
}

func TestBundleRoundTrip(t *testing.T) {
	if len(bundle) == 0 {
		t.Skip("no bundled topics in this build")
	}
	f, err := decodeBundle(bundle)
	if err != nil || len(f.Topics) < 500 || f.Version == "" {
		t.Fatalf("bundle: %d topics, version %q, %v", len(f.Topics), f.Version, err)
	}
}

// zipOf writes files into a zip and returns its path.
func zipOf(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return p
}

func TestRxNormAndLabels(t *testing.T) {
	r := openTest(t)
	conso := strings.Join([]string{
		"83367|ENG||||||1||||RXNORM|IN|83367|atorvastatin||N||",
		"153165|ENG||||||2||||RXNORM|BN|153165|Lipitor||N||",
		"617312|ENG||||||3||||RXNORM|SCD|617312|atorvastatin 20 MG Oral Tablet||N||",
		"617318|ENG||||||4||||RXNORM|SBD|617318|atorvastatin 20 MG Oral Tablet [Lipitor]||N||",
		"5640|ENG||||||5||||RXNORM|IN|5640|ibuprofen||N||",
		"2551|ENG||||||6||||RXNORM|IN|2551|clarithromycin||N||",
	}, "\n") + "\n"
	rel := "153165|||RO|83367|||tradename_of||||||||\n"
	if err := r.importRxNorm(zipOf(t, map[string]string{"rrf/RXNCONSO.RRF": conso, "rrf/RXNREL.RRF": rel}), "2026-10-05"); err != nil {
		t.Fatal(err)
	}
	d, err := r.LookupDrug("Lipitor 20 mg tablet")
	if err != nil || d == nil || !d.IsBrand || len(d.Ingredients) != 1 || d.Ingredients[0] != "atorvastatin" || len(d.Products) != 1 {
		t.Fatalf("brand: %+v %v", d, err)
	}
	d, _ = r.LookupDrug("atorvastatin")
	if d == nil || len(d.Brands) != 1 || d.Brands[0] != "Lipitor" || len(d.Products) != 2 {
		t.Fatalf("ingredient: %+v", d)
	}

	// Labels, as openFDA ships them.
	part := zipOf(t, map[string]string{"drug-label-0001-of-0001.json": `{"meta":{"disclaimer":"x","results":{"skip":0,"limit":4,"total":4}},"results":[
	  {"effective_time":"20200101","drug_interactions":["Old text."],"openfda":{"generic_name":["ATORVASTATIN CALCIUM"],"brand_name":["Lipitor"]}},
	  {"effective_time":"20240101","indications_and_usage":["Lowers LDL."],"drug_interactions":["7 DRUG INTERACTIONS. Clarithromycin raises atorvastatin levels; avoid more than 20 mg. Grapefruit juice too."],"openfda":{"generic_name":["ATORVASTATIN CALCIUM"],"brand_name":["Lipitor","Atorvaliq"]}},
	  {"effective_time":"20250101","openfda":{"generic_name":["ATORVASTATIN CALCIUM"]}},
	  {"effective_time":"20240101","drug_interactions":["Aspirin may reduce its effect."],"openfda":{"generic_name":["IBUPROFEN"],"brand_name":["Advil"]}}
	]}`})
	kept := map[string]keptLabel{}
	if err := r.tx(func(tx *sql.Tx) error {
		return readLabelPart(part, func(l label) error { return keepLabel(tx, l, kept) })
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO labels_fts(rowid, generic, brands) SELECT id, generic, brands FROM labels; INSERT INTO sources(source, version, updated, items) VALUES('openfda', 'x', 'x', 2)`); err != nil {
		t.Fatal(err)
	}
	d, _ = r.LookupDrug("Lipitor")
	if d == nil || d.Label == nil || !strings.Contains(d.Label.Interactions, "Clarithromycin") || d.Label.Effective != "20240101" {
		t.Fatalf("label: %+v", d.Label)
	}
	found, noLabel, err := r.Interactions([]string{"Lipitor", "clarithromycin", "ibuprofen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Label != "Lipitor" || found[0].Mentions != "clarithromycin" || !strings.Contains(found[0].Passages[0], "avoid more than 20 mg") {
		t.Fatalf("interactions: %+v", found)
	}
	if len(noLabel) != 1 || noLabel[0] != "clarithromycin" {
		t.Fatalf("no label: %v", noLabel)
	}

	// A label that names the class, not the drug.
	if _, err := r.db.Exec(`INSERT INTO labels(generic, brands, effective, interactions) VALUES('lisinopril', 'Zestril', '20240101',
		'NSAIDs, including selective COX-2 inhibitors, may reduce the effect of ACE inhibitors and worsen kidney function. Lithium levels may rise.')`); err != nil {
		t.Fatal(err)
	}
	found, _, _ = r.Interactions([]string{"lisinopril", "ibuprofen"})
	if len(found) != 1 || found[0].Label != "lisinopril" || found[0].Mentions != "ibuprofen" || !strings.Contains(found[0].Passages[0], "NSAIDs") {
		t.Fatalf("class mention: %+v", found)
	}
}

func TestLOINC(t *testing.T) {
	r := openTest(t)
	csv := "\"LOINC_NUM\",\"COMPONENT\",\"SHORTNAME\",\"LONG_COMMON_NAME\",\"CLASS\",\"CLASSTYPE\",\"STATUS\",\"EXAMPLE_UCUM_UNITS\",\"RELATEDNAMES2\",\"EXTERNAL_COPYRIGHT_NOTICE\"\n" +
		"\"4548-4\",\"Hemoglobin A1c/Hemoglobin.total\",\"HbA1c MFr Bld\",\"Hemoglobin A1c/Hemoglobin.total in Blood\",\"CHEM\",\"1\",\"ACTIVE\",\"%\",\"A1c; Glycated hemoglobin\",\"\"\n" +
		"\"9999-9\",\"Thing\",\"T\",\"Licensed thing\",\"CHEM\",\"1\",\"ACTIVE\",\"\",\"\",\"Copyright someone\"\n" +
		"\"8302-2\",\"Body height\",\"Body height\",\"Body height\",\"BDYHGT\",\"2\",\"ACTIVE\",\"cm\",\"\",\"\"\n"
	n, err := r.ImportLOINC(zipOf(t, map[string]string{"Loinc_2.81/LoincTable/Loinc.csv": csv}), "Loinc_2.81.zip")
	if err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	tests, err := r.LabTests("glycated hemoglobin", 3)
	if err != nil || len(tests) != 1 || tests[0].Code != "4548-4" {
		t.Fatalf("lab tests: %+v %v", tests, err)
	}
	if r.Sources()[3].Version != "2.81" {
		t.Fatalf("version: %+v", r.Sources()[3])
	}
}
