package store

import (
	"database/sql"
	"errors"
	"strings"
)

// Document is one original file and what's known about it.
type Document struct {
	ID           int64  `json:"id"`
	PersonID     int64  `json:"person_id"`
	Blob         string `json:"-"`
	Filename     string `json:"filename"`
	Mime         string `json:"mime"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Kind         string `json:"kind"`  // lab, imaging, visit, prescription, vaccination, bill, device, other
	Title        string `json:"title"` // a readable name, once known
	DocDate      string `json:"doc_date"`
	Provider     string `json:"provider"`
	Summary      string `json:"summary"`
	Status       string `json:"status"` // new, reading, done, review, failed
	ReviewReason string `json:"review_reason"`
	Extracted    string `json:"-"` // the reader's JSON; sent separately where needed
	Text         string `json:"-"`
	RecipeID     *int64 `json:"recipe_id,omitempty"` // the import recipe that read it
	Added        string `json:"added"`
}

// Document statuses.
const (
	StatusNew     = "new"
	StatusReading = "reading"
	StatusDone    = "done"
	StatusReview  = "review"
	StatusFailed  = "failed"
)

// ErrDuplicate means the same file is already on that profile.
var ErrDuplicate = errors.New("already added")

// docCols leaves out text, which can be large; Document and DocumentText
// fetch it when it's wanted.
const docCols = `id, person_id, blob, filename, mime, size, sha256, kind, title, doc_date, provider, summary, status, review_reason, extracted, recipe_id, added`

func scanDoc(row interface{ Scan(...any) error }) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.PersonID, &d.Blob, &d.Filename, &d.Mime, &d.Size, &d.SHA256,
		&d.Kind, &d.Title, &d.DocDate, &d.Provider, &d.Summary, &d.Status, &d.ReviewReason, &d.Extracted, &d.RecipeID, &d.Added)
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

// AddDocumentTx records a document with everything already known about
// it (an imaging study's, say), returning its id.
func AddDocumentTx(tx *sql.Tx, d Document) (int64, error) {
	res, err := tx.Exec(`INSERT INTO documents(person_id, blob, filename, mime, size, sha256, kind, title, doc_date, provider, summary, status, review_reason, added)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.PersonID, d.Blob, d.Filename, d.Mime, d.Size, d.SHA256, d.Kind, d.Title,
		d.DocDate, d.Provider, d.Summary, d.Status, d.ReviewReason, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Document(id int64) (Document, error) {
	d, err := scanDoc(s.db.QueryRow(`SELECT `+docCols+` FROM documents WHERE id = ?`, id))
	if err != nil {
		return d, err
	}
	err = s.db.QueryRow(`SELECT text FROM documents WHERE id = ?`, id).Scan(&d.Text)
	return d, err
}

// Documents lists a profile's files, newest first. q filters on the
// filename, title, kind, summary and the text read from it.
func (s *Store) Documents(personID int64, q string, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 500
	}
	where := `person_id = ?`
	args := []any{personID}
	if q = strings.TrimSpace(q); q != "" {
		where += ` AND (filename LIKE ? OR title LIKE ? OR kind LIKE ? OR summary LIKE ? OR provider LIKE ? OR text LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like, like)
	}
	args = append(args, limit)
	return s.docs(`SELECT `+docCols+` FROM documents WHERE `+where+` ORDER BY added DESC, id DESC LIMIT ?`, args...)
}

func (s *Store) docs(query string, args ...any) ([]Document, error) {
	rows, err := s.db.Query(query, args...)
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

// NextToRead is the oldest file waiting to be read, on any profile.
func (s *Store) NextToRead() (Document, error) {
	return scanDoc(s.db.QueryRow(`SELECT ` + docCols + ` FROM documents WHERE status = 'new' ORDER BY id LIMIT 1`))
}

// NextExport is the oldest health export waiting to be imported.
func (s *Store) NextExport() (Document, error) {
	return scanDoc(s.db.QueryRow(`SELECT ` + docCols + ` FROM documents WHERE status = 'new' AND mime LIKE 'application/x-health-export%' ORDER BY id LIMIT 1`))
}

// ResetReading puts files left half-read (the app stopped) back in line.
func (s *Store) ResetReading() error {
	_, err := s.db.Exec(`UPDATE documents SET status = 'new' WHERE status = 'reading'`)
	return err
}

// Waiting lists files in a status across every profile.
func (s *Store) Waiting(status string) ([]Document, error) {
	return s.docs(`SELECT `+docCols+` FROM documents WHERE status = ? ORDER BY id`, status)
}

// Counts is how many files each profile has in each status.
func (s *Store) Counts() (map[int64]map[string]int, error) {
	rows, err := s.db.Query(`SELECT person_id, status, count(*) FROM documents GROUP BY person_id, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var pid int64
		var st string
		var n int
		if err := rows.Scan(&pid, &st, &n); err != nil {
			return nil, err
		}
		if out[pid] == nil {
			out[pid] = map[string]int{}
		}
		out[pid][st] = n
	}
	return out, rows.Err()
}

// SetStatus changes only the status and its reason.
func (s *Store) SetStatus(id int64, status, reason string) error {
	_, err := s.db.Exec(`UPDATE documents SET status = ?, review_reason = ? WHERE id = ?`, status, reason, id)
	return err
}

// UpdateDocument saves every field the reader or a person can change.
func (s *Store) UpdateDocument(d Document) error {
	return s.Tx(func(tx *sql.Tx) error { return UpdateDocumentTx(tx, d) })
}

func UpdateDocumentTx(tx *sql.Tx, d Document) error {
	_, err := tx.Exec(`UPDATE documents SET person_id = ?, kind = ?, title = ?, doc_date = ?, provider = ?, summary = ?,
		status = ?, review_reason = ?, extracted = ?, text = ? WHERE id = ?`,
		d.PersonID, d.Kind, d.Title, d.DocDate, d.Provider, d.Summary, d.Status, d.ReviewReason, d.Extracted, d.Text, d.ID)
	return err
}

func (s *Store) DeleteDocument(id int64) error {
	_, err := s.db.Exec(`DELETE FROM documents WHERE id = ?`, id)
	return err
}
