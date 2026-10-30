package store

import (
	"database/sql"
	"errors"
	"strings"
)

// Document is one original file and what's known about it.
type Document struct {
	ID       int64  `json:"id"`
	PersonID int64  `json:"person_id"`
	Blob     string `json:"-"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Kind     string `json:"kind"`  // lab, imaging, visit, prescription, vaccination, bill, device, other
	Title    string `json:"title"` // a readable name, once known
	DocDate  string `json:"doc_date"`
	Summary  string `json:"summary"`
	Status   string `json:"status"` // new, reading, done, review, failed
	Added    string `json:"added"`
}

// ErrDuplicate means the same file is already on that profile.
var ErrDuplicate = errors.New("already added")

const docCols = `id, person_id, blob, filename, mime, size, sha256, kind, title, doc_date, summary, status, added`

func scanDoc(row interface{ Scan(...any) error }) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.PersonID, &d.Blob, &d.Filename, &d.Mime, &d.Size, &d.SHA256,
		&d.Kind, &d.Title, &d.DocDate, &d.Summary, &d.Status, &d.Added)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// AddDocument records a stored file. It returns ErrDuplicate, with the
// existing document, when that profile already has the same bytes.
func (s *Store) AddDocument(d Document) (Document, error) {
	if old, err := scanDoc(s.db.QueryRow(`SELECT `+docCols+` FROM documents WHERE person_id = ? AND sha256 = ?`, d.PersonID, d.SHA256)); err == nil {
		return old, ErrDuplicate
	}
	res, err := s.db.Exec(`INSERT INTO documents(person_id, blob, filename, mime, size, sha256, status, added)
		VALUES(?, ?, ?, ?, ?, ?, 'new', ?)`, d.PersonID, d.Blob, d.Filename, d.Mime, d.Size, d.SHA256, now())
	if err != nil {
		return d, err
	}
	id, _ := res.LastInsertId()
	return s.Document(id)
}

func (s *Store) Document(id int64) (Document, error) {
	return scanDoc(s.db.QueryRow(`SELECT `+docCols+` FROM documents WHERE id = ?`, id))
}

// Documents lists a profile's files, newest first. q filters on the
// filename, title, kind and summary.
func (s *Store) Documents(personID int64, q string, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 500
	}
	where := `person_id = ?`
	args := []any{personID}
	if q = strings.TrimSpace(q); q != "" {
		where += ` AND (filename LIKE ? OR title LIKE ? OR kind LIKE ? OR summary LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like, like)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT `+docCols+` FROM documents WHERE `+where+` ORDER BY added DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDocument saves the editable fields.
func (s *Store) UpdateDocument(d Document) error {
	_, err := s.db.Exec(`UPDATE documents SET person_id = ?, kind = ?, title = ?, doc_date = ?, summary = ?, status = ? WHERE id = ?`,
		d.PersonID, d.Kind, d.Title, d.DocDate, d.Summary, d.Status, d.ID)
	return err
}

func (s *Store) DeleteDocument(id int64) error {
	_, err := s.db.Exec(`DELETE FROM documents WHERE id = ?`, id)
	return err
}
