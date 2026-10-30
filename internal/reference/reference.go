// Package reference keeps public medical reference data on the server, so
// the assistant can look things up instead of relying on what a small
// model remembers:
//
//   - MedlinePlus health topics (National Library of Medicine, public
//     domain): plain-language summaries of conditions, tests and terms.
//     Bundled into the binary; can be updated.
//   - RxNorm Current Prescribable Content (NLM, public domain): drug names,
//     ingredients, brands and strengths. Downloaded on request.
//   - openFDA drug labels (FDA, CC0): indications, warnings and the
//     interactions section of each label. Downloaded on request.
//   - LOINC (Regenstrief Institute, free with its license): standard lab
//     test names. Dropped in by the user, who downloads it with their own
//     loinc.org account.
//
// None of it is personal, so it lives in its own unencrypted database, and
// nothing about what is looked up is stored or sent anywhere.
package reference

import (
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

type Ref struct {
	db *sql.DB
}

// Sources, in the order Settings lists them.
const (
	MedlinePlus = "medlineplus"
	RxNorm      = "rxnorm"
	Labels      = "openfda"
	LOINC       = "loinc"
)

// Credits are what each source asks to be shown wherever it is used.
var Credits = map[string]string{
	MedlinePlus: "Source: MedlinePlus, National Library of Medicine.",
	RxNorm:      "Drug names: RxNorm, National Library of Medicine.",
	Labels:      "Drug labels: data provided by the U.S. Food and Drug Administration (open.fda.gov).",
	LOINC:       "This material contains content from LOINC (http://loinc.org). LOINC is copyright © Regenstrief Institute, Inc. and the Logical Observation Identifiers Names and Codes (LOINC) Committee and is available at no cost under the license at http://loinc.org/license. LOINC® is a registered United States trademark of Regenstrief Institute, Inc.",
}

const schema = `
CREATE TABLE IF NOT EXISTS sources (
  source  TEXT PRIMARY KEY,
  version TEXT NOT NULL,   -- the release date or name
  updated TEXT NOT NULL,   -- when it was loaded here
  items   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS topics (
  id      INTEGER PRIMARY KEY,
  title   TEXT NOT NULL,
  url     TEXT NOT NULL,
  also    TEXT NOT NULL DEFAULT '',   -- other names, one per line
  summary TEXT NOT NULL,
  groups  TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE IF NOT EXISTS topics_fts USING fts5(title, also, summary, content='topics', content_rowid='id', tokenize='porter');
CREATE TABLE IF NOT EXISTS drugs (
  rxcui TEXT NOT NULL,
  tty   TEXT NOT NULL,            -- IN ingredient, BN brand, SCD/SBD a product with its strength…
  name  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS drugs_rxcui ON drugs(rxcui);
CREATE INDEX IF NOT EXISTS drugs_name ON drugs(name COLLATE NOCASE);
CREATE VIRTUAL TABLE IF NOT EXISTS drugs_fts USING fts5(name, content='drugs', tokenize='unicode61');
CREATE TABLE IF NOT EXISTS brands (
  brand      TEXT NOT NULL,       -- rxcui of the brand (BN)
  ingredient TEXT NOT NULL        -- rxcui of the ingredient (IN)
);
CREATE INDEX IF NOT EXISTS brands_brand ON brands(brand);
CREATE INDEX IF NOT EXISTS brands_ingredient ON brands(ingredient);
CREATE TABLE IF NOT EXISTS labels (
  id                INTEGER PRIMARY KEY,
  generic           TEXT NOT NULL,   -- lower case
  brands            TEXT NOT NULL,   -- one per line
  effective         TEXT NOT NULL,
  indications       TEXT NOT NULL DEFAULT '',
  boxed             TEXT NOT NULL DEFAULT '',
  contraindications TEXT NOT NULL DEFAULT '',
  warnings          TEXT NOT NULL DEFAULT '',
  interactions      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS labels_generic ON labels(generic);
CREATE VIRTUAL TABLE IF NOT EXISTS labels_fts USING fts5(generic, brands, content='labels', content_rowid='id');
CREATE TABLE IF NOT EXISTS loinc (
  code      TEXT PRIMARY KEY,
  component TEXT NOT NULL,
  name      TEXT NOT NULL,
  short     TEXT NOT NULL DEFAULT '',
  class     TEXT NOT NULL DEFAULT '',
  units     TEXT NOT NULL DEFAULT '',
  related   TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE IF NOT EXISTS loinc_fts USING fts5(name, short, component, related, content='loinc', content_rowid='rowid');
`

// Open opens (or creates) reference.db in dir, loading the bundled
// MedlinePlus topics if they're newer than what's there.
func Open(dir string) (*Ref, error) {
	path := filepath.ToSlash(filepath.Join(dir, "reference.db"))
	// FTS5 (full-text search) is an extension in this SQLite build.
	db, err := driver.Open("file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)", fts5.Register)
	if err != nil {
		return nil, err
	}
	// Several connections: in WAL mode lookups keep reading the old data
	// while a long download writes the new.
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	r := &Ref{db: db}
	if err := r.loadBundled(); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Ref) Close() error { return r.db.Close() }

// Source is what Settings shows about one source.
type Source struct {
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
	Updated string `json:"updated,omitempty"`
	Items   int    `json:"items"`
	Credit  string `json:"credit"`
}

// Sources lists every source and what's loaded of it.
func (r *Ref) Sources() []Source {
	out := []Source{}
	for _, s := range []string{MedlinePlus, RxNorm, Labels, LOINC} {
		src := Source{Source: s, Credit: Credits[s]}
		r.db.QueryRow(`SELECT version, updated, items FROM sources WHERE source = ?`, s).Scan(&src.Version, &src.Updated, &src.Items)
		out = append(out, src)
	}
	return out
}

// Has reports whether a source is loaded.
func (r *Ref) Has(source string) bool {
	var n int
	r.db.QueryRow(`SELECT items FROM sources WHERE source = ?`, source).Scan(&n)
	return n > 0
}

func (r *Ref) version(source string) string {
	var v string
	r.db.QueryRow(`SELECT version FROM sources WHERE source = ?`, source).Scan(&v)
	return v
}

func setSource(tx *sql.Tx, source, version string, items int) error {
	_, err := tx.Exec(`INSERT INTO sources(source, version, updated, items) VALUES(?, ?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET version = excluded.version, updated = excluded.updated, items = excluded.items`,
		source, version, time.Now().UTC().Format(time.RFC3339), items)
	return err
}

func (r *Ref) tx(f func(tx *sql.Tx) error) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ErrMissing means a lookup needs a source that isn't loaded.
var ErrMissing = errors.New("not downloaded")
