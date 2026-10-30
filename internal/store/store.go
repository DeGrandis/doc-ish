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
	_ "github.com/ncruces/go-sqlite3/embed"
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

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
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
