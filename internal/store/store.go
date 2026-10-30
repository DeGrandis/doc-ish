// Package store keeps everything except the original files in one SQLite
// database, encrypted page by page with the data key (Adiantum, through
// ncruces/go-sqlite3's VFS). Without the key the file is noise.
package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
)

type Store struct {
	db *sql.DB
}

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// Open opens (or creates) the database in dir with the 32-byte data key.
func Open(dir string, key []byte) (*Store, error) {
	path := filepath.ToSlash(filepath.Join(dir, "records.db"))
	q := url.Values{}
	q.Set("vfs", "adiantum")
	q.Set("hexkey", hex.EncodeToString(key))
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "temp_store(memory)")
	db, err := sql.Open("sqlite3", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS people (
  id      INTEGER PRIMARY KEY,
  name    TEXT NOT NULL,
  color   TEXT NOT NULL DEFAULT '',
  profile TEXT NOT NULL DEFAULT '{}',
  created TEXT NOT NULL,
  updated TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS documents (
  id        INTEGER PRIMARY KEY,
  person_id INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
  blob      TEXT NOT NULL,
  filename  TEXT NOT NULL,
  mime      TEXT NOT NULL DEFAULT '',
  size      INTEGER NOT NULL DEFAULT 0,
  sha256    TEXT NOT NULL,
  kind      TEXT NOT NULL DEFAULT '',
  title     TEXT NOT NULL DEFAULT '',
  doc_date  TEXT NOT NULL DEFAULT '',
  summary   TEXT NOT NULL DEFAULT '',
  status    TEXT NOT NULL DEFAULT 'new',
  added     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS documents_person ON documents(person_id, added);
CREATE UNIQUE INDEX IF NOT EXISTS documents_dedupe ON documents(person_id, sha256);
-- Anything with a value: weight, a lab result, blood pressure, a symptom
-- score. One table, so any of it can be charted on request.
CREATE TABLE IF NOT EXISTS observations (
  id          INTEGER PRIMARY KEY,
  person_id   INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  code        TEXT NOT NULL DEFAULT '',
  value       REAL,
  value_text  TEXT NOT NULL DEFAULT '',
  unit        TEXT NOT NULL DEFAULT '',
  ref_low     REAL,
  ref_high    REAL,
  observed    TEXT NOT NULL,
  document_id INTEGER REFERENCES documents(id) ON DELETE SET NULL,
  source      TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  added       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS observations_person ON observations(person_id, name, observed);
CREATE TABLE IF NOT EXISTS chats (
  id        INTEGER PRIMARY KEY,
  person_id INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
  title     TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  updated   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id      INTEGER PRIMARY KEY,
  chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  role    TEXT NOT NULL,
  content TEXT NOT NULL,
  created TEXT NOT NULL
);
`

// Every change since the first schema, applied once each. Columns are
// added only when missing, so this is safe to run on every open.
var columns = []struct{ table, col, def string }{
	{"documents", "text", "TEXT NOT NULL DEFAULT ''"},          // extracted text, for search and the assistant
	{"documents", "extracted", "TEXT NOT NULL DEFAULT ''"},     // the reader's JSON result
	{"documents", "review_reason", "TEXT NOT NULL DEFAULT ''"}, // why it needs a look, or why reading failed
	{"documents", "provider", "TEXT NOT NULL DEFAULT ''"},
	{"observations", "flag", "TEXT NOT NULL DEFAULT ''"}, // H, L, abnormal… as printed
	{"activity", "document_id", "INTEGER"},               // the file a change was about
	{"messages", "extra", "TEXT NOT NULL DEFAULT ''"},    // charts, opened files and changes shown with an answer
	{"documents", "recipe_id", "INTEGER"},                // the import recipe that read it
}

const activitySchema = `
CREATE TABLE IF NOT EXISTS activity (
  id        INTEGER PRIMARY KEY,
  person_id INTEGER REFERENCES people(id) ON DELETE CASCADE,
  at        TEXT NOT NULL,
  actor     TEXT NOT NULL,       -- you, reader, assistant
  summary   TEXT NOT NULL,
  undo      TEXT NOT NULL DEFAULT '',
  undone    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS activity_person ON activity(person_id, id);
-- Import recipes: Starlark the model wrote for one file format, used for
-- every file in that format once someone approves it.
CREATE TABLE IF NOT EXISTS recipes (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  fingerprint TEXT NOT NULL,
  script      TEXT NOT NULL,
  status      TEXT NOT NULL,        -- proposed, approved, rejected
  document_id INTEGER,              -- the file it was written for
  preview     TEXT NOT NULL DEFAULT '',
  created     TEXT NOT NULL,
  approved    TEXT NOT NULL DEFAULT '',
  uses        INTEGER NOT NULL DEFAULT 0,
  last_used   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS recipes_fingerprint ON recipes(fingerprint, status);
CREATE INDEX IF NOT EXISTS observations_document ON observations(document_id);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema + activitySchema); err != nil {
		return err
	}
	for _, c := range columns {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.col).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.col + ` ` + c.def); err != nil {
				return fmt.Errorf("add %s.%s: %w", c.table, c.col, err)
			}
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS activity_document ON activity(document_id, id)`)
	return err
}

// Tx runs f in a transaction, committing if it returns nil.
func (s *Store) Tx(f func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------- settings ----------

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
