package reference

import (
	"database/sql"
	"regexp"
	"strings"
	"unicode"
)

// ftsQuery turns words into an FTS5 query: each word quoted (so nothing
// in them is read as query syntax), all required, or any with or=true.
func ftsQuery(q string, or bool) string {
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 {
			words = append(words, `"`+w+`"`)
		}
	}
	if or {
		return strings.Join(words, " OR ")
	}
	return strings.Join(words, " ")
}

// ---------- MedlinePlus ----------

// Explain finds the MedlinePlus topics that best match a condition, test
// or term: a title or other name that matches exactly first, then a
// full-text search.
func (r *Ref) Explain(q string, limit int) ([]Topic, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	var ids []int64
	rows, err := r.db.Query(`SELECT id FROM topics WHERE title = ? COLLATE NOCASE OR ('\n' || also || '\n') LIKE ? LIMIT ?`,
		q, "%\n"+q+"\n%", limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, or := range []bool{false, true} {
		if len(ids) >= limit {
			break
		}
		fq := ftsQuery(q, or)
		if fq == "" {
			break
		}
		rows, err := r.db.Query(`SELECT rowid FROM topics_fts WHERE topics_fts MATCH ? ORDER BY bm25(topics_fts, 10.0, 6.0, 1.0) LIMIT ?`, fq, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			if !contains(ids, id) {
				ids = append(ids, id)
			}
		}
		rows.Close()
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	var out []Topic
	for _, id := range ids {
		var t Topic
		var also, groups string
		if err := r.db.QueryRow(`SELECT id, title, url, also, summary, groups FROM topics WHERE id = ?`, id).
			Scan(&t.ID, &t.Title, &t.URL, &also, &t.Summary, &groups); err == nil {
			t.Also, t.Groups = lines(also), lines(groups)
			out = append(out, t)
		}
	}
	return out, nil
}

func contains(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ---------- drugs ----------

// Drug is what the reference knows about a medication.
type Drug struct {
	Query       string     `json:"query"`
	Name        string     `json:"name"`
	IsBrand     bool       `json:"is_brand"`
	Ingredients []string   `json:"ingredients,omitempty"`
	Brands      []string   `json:"brands,omitempty"`
	Products    []string   `json:"products,omitempty"`
	Label       *DrugLabel `json:"label,omitempty"`
}

// DrugLabel is the kept sections of an FDA label.
type DrugLabel struct {
	Generic           string `json:"generic"`
	Brands            string `json:"brands"`
	Effective         string `json:"effective"`
	Indications       string `json:"indications,omitempty"`
	Boxed             string `json:"boxed,omitempty"`
	Contraindications string `json:"contraindications,omitempty"`
	Warnings          string `json:"warnings,omitempty"`
	Interactions      string `json:"interactions,omitempty"`
}

var doseRE = regexp.MustCompile(`(?i)\s+\d[\d.,]*\s*(mg|mcg|g|ml|iu|units?|%)\b.*$`)

// cleanDrugName drops a dose and form typed after a name ("Lipitor 20 mg
// tablet" -> "Lipitor").
func cleanDrugName(q string) string {
	return strings.TrimSpace(doseRE.ReplaceAllString(strings.TrimSpace(q), ""))
}

// LookupDrug finds a medication by ingredient or brand name. With RxNorm
// loaded it knows ingredients, brands and products; with the labels
// loaded it adds the label. Either alone is enough to answer.
func (r *Ref) LookupDrug(q string) (*Drug, error) {
	name := cleanDrugName(q)
	if name == "" {
		return nil, nil
	}
	d := &Drug{Query: q, Name: name}
	found := false
	if r.Has(RxNorm) {
		rxcui, tty, display := r.findConcept(name)
		if rxcui != "" {
			found = true
			d.Name = display
			if tty == "BN" {
				d.IsBrand = true
				d.Brands = []string{display}
				d.Ingredients = r.names(`SELECT d.name FROM brands b JOIN drugs d ON d.rxcui = b.ingredient AND d.tty IN ('IN','PIN','MIN') WHERE b.brand = ?`, rxcui)
			} else {
				d.Ingredients = []string{display}
				d.Brands = r.names(`SELECT d.name FROM brands b JOIN drugs d ON d.rxcui = b.brand AND d.tty = 'BN' WHERE b.ingredient = ?`, rxcui)
			}
			like := "%" + strings.ToLower(display) + "%"
			if d.IsBrand {
				like = "%[" + strings.ToLower(display) + "]%"
			}
			d.Products = r.names(`SELECT name FROM drugs WHERE tty IN ('SCD','SBD') AND lower(name) LIKE ? ORDER BY tty, length(name), name LIMIT 15`, like)
		}
	}
	if r.Has(Labels) {
		candidates := append([]string{}, d.Ingredients...)
		candidates = append(candidates, name)
		for _, c := range candidates {
			if l := r.label(c); l != nil {
				d.Label = l
				found = true
				break
			}
		}
	}
	if !found {
		return nil, nil
	}
	return d, nil
}

// findConcept finds an ingredient or brand by exact name, then by search.
func (r *Ref) findConcept(name string) (rxcui, tty, display string) {
	err := r.db.QueryRow(`SELECT rxcui, tty, name FROM drugs WHERE name = ? COLLATE NOCASE AND tty IN ('IN','PIN','MIN','BN')
		ORDER BY CASE tty WHEN 'IN' THEN 0 WHEN 'BN' THEN 1 ELSE 2 END LIMIT 1`, name).Scan(&rxcui, &tty, &display)
	if err == nil {
		return
	}
	fq := ftsQuery(name, false)
	if fq == "" {
		return "", "", ""
	}
	r.db.QueryRow(`SELECT d.rxcui, d.tty, d.name FROM drugs_fts f JOIN drugs d ON d.rowid = f.rowid
		WHERE drugs_fts MATCH ? AND d.tty IN ('IN','PIN','MIN','BN') ORDER BY bm25(drugs_fts), length(d.name) LIMIT 1`, fq).Scan(&rxcui, &tty, &display)
	return
}

func (r *Ref) names(query string, args ...any) []string {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		if !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	return out
}

// label finds the kept label by generic name, then by brand.
func (r *Ref) label(name string) *DrugLabel {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return nil
	}
	var l DrugLabel
	scan := func(row *sql.Row) bool {
		return row.Scan(&l.Generic, &l.Brands, &l.Effective, &l.Indications, &l.Boxed, &l.Contraindications, &l.Warnings, &l.Interactions) == nil
	}
	const cols = `generic, brands, effective, indications, boxed, contraindications, warnings, interactions`
	if scan(r.db.QueryRow(`SELECT `+cols+` FROM labels WHERE generic = ?`, n)) {
		return &l
	}
	if scan(r.db.QueryRow(`SELECT `+cols+` FROM labels WHERE ('\n' || lower(brands) || '\n') LIKE ? ORDER BY length(generic) LIMIT 1`, "%\n"+n+"\n%")) {
		return &l
	}
	// "atorvastatin" also finds "atorvastatin calcium".
	if scan(r.db.QueryRow(`SELECT `+cols+` FROM labels WHERE generic LIKE ? ORDER BY length(generic) LIMIT 1`, n+" %")) {
		return &l
	}
	return nil
}

// Mention is a passage in one drug's label that names another drug.
type Mention struct {
	Label    string   `json:"label"`    // the drug whose label it is
	Mentions string   `json:"mentions"` // the drug it names
	Passages []string `json:"passages"`
}

// Interactions looks in each drug's label for the others by name. It
// returns what the labels say, the drugs with no label, and the drugs it
// couldn't identify. A label that names no other drug is not proof the
// combination is safe: labels often speak of classes ("statins", "CYP3A4
// inhibitors") rather than names.
func (r *Ref) Interactions(drugs []string) (found []Mention, noLabel []string, err error) {
	if !r.Has(Labels) {
		return nil, nil, ErrMissing
	}
	type med struct {
		name    string
		aliases []string
		label   *DrugLabel
	}
	var meds []med
	for _, q := range drugs {
		q = cleanDrugName(q)
		if q == "" {
			continue
		}
		m := med{name: q, aliases: []string{q}}
		if d, _ := r.LookupDrug(q); d != nil {
			m.aliases = append(m.aliases, d.Ingredients...)
			m.aliases = append(m.aliases, d.Brands...)
			m.label = d.Label
		}
		if m.label == nil {
			m.label = r.label(q)
		}
		if m.label != nil {
			m.aliases = append(m.aliases, m.label.Generic)
			m.aliases = append(m.aliases, lines(m.label.Brands)...)
		} else {
			noLabel = append(noLabel, q)
		}
		// Labels often name a class instead: "NSAIDs", "ACE inhibitors".
		m.aliases = append(m.aliases, classPhrases(m.aliases)...)
		meds = append(meds, m)
	}
	for i, a := range meds {
		if a.label == nil || a.label.Interactions == "" {
			continue
		}
		for j, b := range meds {
			if i == j {
				continue
			}
			if ps := passages(a.label.Interactions, b.aliases, 3); len(ps) > 0 {
				found = append(found, Mention{Label: a.name, Mentions: b.name, Passages: ps})
			}
		}
	}
	return found, noLabel, nil
}

var sentenceRE = regexp.MustCompile(`[^.!?\n]+[.!?]?`)

// passages are the sentences of text that name any of the words.
func passages(text string, names []string, limit int) []string {
	var res []*regexp.Regexp
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if len(n) < 3 || seen[n] {
			continue
		}
		seen[n] = true
		res = append(res, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(n)+`\b`))
	}
	var out []string
	for _, s := range sentenceRE.FindAllString(text, -1) {
		s = strings.TrimSpace(s)
		for _, re := range res {
			if re.MatchString(s) {
				if len(s) > 500 {
					s = s[:500] + "…"
				}
				out = append(out, s)
				break
			}
		}
		if len(out) == limit {
			break
		}
	}
	return out
}

// ---------- LOINC ----------

// LabTest is one LOINC lab test.
type LabTest struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Component string `json:"component"`
	Short     string `json:"short,omitempty"`
	Class     string `json:"class,omitempty"`
	Units     string `json:"units,omitempty"`
}

// LabTests finds LOINC tests by name.
func (r *Ref) LabTests(q string, limit int) ([]LabTest, error) {
	if !r.Has(LOINC) {
		return nil, ErrMissing
	}
	var out []LabTest
	for _, or := range []bool{false, true} {
		fq := ftsQuery(q, or)
		if fq == "" || len(out) > 0 {
			break
		}
		rows, err := r.db.Query(`SELECT l.code, l.name, l.component, l.short, l.class, l.units FROM loinc_fts f JOIN loinc l ON l.rowid = f.rowid
			WHERE loinc_fts MATCH ? ORDER BY bm25(loinc_fts, 6.0, 4.0, 3.0, 1.0) LIMIT ?`, fq, limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t LabTest
			rows.Scan(&t.Code, &t.Name, &t.Component, &t.Short, &t.Class, &t.Units)
			out = append(out, t)
		}
		rows.Close()
	}
	return out, nil
}
