package store

import "database/sql"

// Sections are the records inside one long file (a records release: its
// visit notes, lab reports, letters), each with its own pages, kind and
// date, so the timeline shows a visit on its own day.

const sectionSchema = `
CREATE TABLE IF NOT EXISTS sections (
  id          INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  person_id   INTEGER NOT NULL,
  idx         INTEGER NOT NULL,  -- its place in the file, from 1
  first_page  INTEGER NOT NULL,
  last_page   INTEGER NOT NULL,
  kind        TEXT NOT NULL DEFAULT '',
  title       TEXT NOT NULL DEFAULT '',
  date        TEXT NOT NULL DEFAULT '',
  provider    TEXT NOT NULL DEFAULT '',
  summary     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sections_document ON sections(document_id, idx);
CREATE INDEX IF NOT EXISTS sections_person ON sections(person_id, date);
`

type Section struct {
	ID         int64  `json:"id"`
	DocumentID int64  `json:"document_id"`
	PersonID   int64  `json:"person_id"`
	Index      int    `json:"index"`
	FirstPage  int    `json:"first_page"`
	LastPage   int    `json:"last_page"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Date       string `json:"date"`
	Provider   string `json:"provider"`
	Summary    string `json:"summary"`
}

const sectionCols = `id, document_id, person_id, idx, first_page, last_page, kind, title, date, provider, summary`

// AddSectionsTx stores a file's sections, in order, and returns their ids.
func AddSectionsTx(tx *sql.Tx, secs []Section) ([]int64, error) {
	var ids []int64
	for _, s := range secs {
		res, err := tx.Exec(`INSERT INTO sections (document_id, person_id, idx, first_page, last_page, kind, title, date, provider, summary)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, s.DocumentID, s.PersonID, s.Index, s.FirstPage, s.LastPage, s.Kind, s.Title, s.Date, s.Provider, s.Summary)
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// DeleteDocumentSectionsTx removes what was found inside a file, and
// unlinks its values and follow-ups from it.
func DeleteDocumentSectionsTx(tx *sql.Tx, docID int64) error {
	for _, q := range []string{
		`UPDATE observations SET section_id = NULL WHERE section_id IN (SELECT id FROM sections WHERE document_id = ?)`,
		`UPDATE followups SET section_id = NULL WHERE section_id IN (SELECT id FROM sections WHERE document_id = ?)`,
		`DELETE FROM sections WHERE document_id = ?`,
	} {
		if _, err := tx.Exec(q, docID); err != nil {
			return err
		}
	}
	return nil
}

// MoveDocumentSectionsTx gives a file's sections to the file's owner.
func MoveDocumentSectionsTx(tx *sql.Tx, docID, personID int64) error {
	_, err := tx.Exec(`UPDATE sections SET person_id = ? WHERE document_id = ?`, personID, docID)
	return err
}

func (s *Store) sections(where string, args ...any) ([]Section, error) {
	rows, err := s.db.Query(`SELECT `+sectionCols+` FROM sections WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Section{}
	for rows.Next() {
		var x Section
		if err := rows.Scan(&x.ID, &x.DocumentID, &x.PersonID, &x.Index, &x.FirstPage, &x.LastPage, &x.Kind, &x.Title, &x.Date, &x.Provider, &x.Summary); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DocumentSections lists a file's sections in page order.
func (s *Store) DocumentSections(docID int64) ([]Section, error) {
	return s.sections(`document_id = ? ORDER BY idx`, docID)
}

// PersonSections lists every section of a person's files.
func (s *Store) PersonSections(personID int64) ([]Section, error) {
	return s.sections(`person_id = ? ORDER BY document_id, idx`, personID)
}

// SectionDays counts the values in each of a person's sections by the day
// they're from: a cumulative lab table holds several days.
func (s *Store) SectionDays(personID int64) (map[int64]map[string]int, error) {
	rows, err := s.db.Query(`SELECT section_id, substr(observed, 1, 10), count(*) FROM observations
		WHERE person_id = ? AND section_id IS NOT NULL GROUP BY 1, 2`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var id int64
		var day string
		var n int
		if err := rows.Scan(&id, &day, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][day] = n
	}
	return out, rows.Err()
}
