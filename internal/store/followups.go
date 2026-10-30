package store

import (
	"database/sql"
	"errors"
)

// Follow-ups are what a file (or a person) says should happen later: a
// recheck, a repeat scan, a visit. They stay open until done or set aside;
// a later file that shows it done closes one, saying which.

const followUpSchema = `
CREATE TABLE IF NOT EXISTS followups (
  id               INTEGER PRIMARY KEY,
  person_id        INTEGER NOT NULL REFERENCES people(id) ON DELETE CASCADE,
  document_id      INTEGER REFERENCES documents(id) ON DELETE SET NULL, -- the file that said it
  what             TEXT NOT NULL,
  when_text        TEXT NOT NULL DEFAULT '',  -- as written: "in about six months"
  due              TEXT NOT NULL DEFAULT '',  -- YYYY-MM-DD, or empty when the timing is vague
  from_date        TEXT NOT NULL DEFAULT '',  -- the date of the note that said it
  status           TEXT NOT NULL DEFAULT 'open',
  done_date        TEXT NOT NULL DEFAULT '',  -- when it happened (the evidence's date), or was marked
  done_note        TEXT NOT NULL DEFAULT '',  -- what shows it done
  done_document_id INTEGER REFERENCES documents(id) ON DELETE SET NULL,
  source           TEXT NOT NULL DEFAULT '',  -- reader, you, assistant
  added            TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS followups_person ON followups(person_id, status, due);
CREATE INDEX IF NOT EXISTS followups_document ON followups(document_id);
`

const (
	FollowUpOpen      = "open"
	FollowUpDone      = "done"
	FollowUpDismissed = "dismissed"
)

type FollowUp struct {
	ID             int64  `json:"id"`
	PersonID       int64  `json:"person_id"`
	DocumentID     *int64 `json:"document_id"`
	What           string `json:"what"`
	When           string `json:"when"`
	Due            string `json:"due"`
	From           string `json:"from"`
	Status         string `json:"status"`
	DoneDate       string `json:"done_date"`
	DoneNote       string `json:"done_note"`
	DoneDocumentID *int64 `json:"done_document_id"`
	SectionID      *int64 `json:"section_id,omitempty"` // in a long file, the record that said it
	Source         string `json:"source"`
	Added          string `json:"added"`
}

const followUpCols = `id, person_id, document_id, what, when_text, due, from_date, status, done_date, done_note, done_document_id, source, added, section_id`

func scanFollowUp(row interface{ Scan(...any) error }) (FollowUp, error) {
	var f FollowUp
	var doc, done sql.NullInt64
	err := row.Scan(&f.ID, &f.PersonID, &doc, &f.What, &f.When, &f.Due, &f.From, &f.Status, &f.DoneDate, &f.DoneNote, &done, &f.Source, &f.Added, &f.SectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	if doc.Valid {
		f.DocumentID = &doc.Int64
	}
	if done.Valid {
		f.DoneDocumentID = &done.Int64
	}
	return f, err
}

func AddFollowUpTx(tx *sql.Tx, f FollowUp) (int64, error) {
	if f.Status == "" {
		f.Status = FollowUpOpen
	}
	res, err := tx.Exec(`INSERT INTO followups (person_id, document_id, what, when_text, due, from_date, status, done_date, done_note, done_document_id, source, added, section_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.PersonID, f.DocumentID, f.What, f.When, f.Due, f.From, f.Status, f.DoneDate, f.DoneNote, f.DoneDocumentID, f.Source, now(), f.SectionID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateFollowUpTx saves a follow-up's text, timing and status.
func UpdateFollowUpTx(tx *sql.Tx, f FollowUp) error {
	_, err := tx.Exec(`UPDATE followups SET what = ?, when_text = ?, due = ?, status = ?, done_date = ?, done_note = ?, done_document_id = ? WHERE id = ?`,
		f.What, f.When, f.Due, f.Status, f.DoneDate, f.DoneNote, f.DoneDocumentID, f.ID)
	return err
}

func FollowUpTx(tx *sql.Tx, id int64) (FollowUp, error) {
	return scanFollowUp(tx.QueryRow(`SELECT `+followUpCols+` FROM followups WHERE id = ?`, id))
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func followUps(q queryer, where string, args ...any) ([]FollowUp, error) {
	rows, err := q.Query(`SELECT `+followUpCols+` FROM followups WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FollowUp{}
	for rows.Next() {
		f, err := scanFollowUp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowUps lists a person's follow-ups: open ones by due date (undated
// last), then the rest, latest first.
func (s *Store) FollowUps(personID int64) ([]FollowUp, error) {
	return followUps(s.db, `person_id = ? ORDER BY status != 'open', CASE WHEN status = 'open' THEN due = '' END, CASE WHEN status = 'open' THEN due END, done_date DESC, id DESC`, personID)
}

// OpenFollowUpsTx lists a person's open follow-ups.
func OpenFollowUpsTx(tx *sql.Tx, personID int64) ([]FollowUp, error) {
	return followUps(tx, `person_id = ? AND status = 'open' ORDER BY id`, personID)
}

func DeleteFollowUpsTx(tx *sql.Tx, ids []int64) error {
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM followups WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ReopenFollowUpsTx opens follow-ups again, as if never closed.
func ReopenFollowUpsTx(tx *sql.Tx, ids []int64) error {
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE followups SET status = 'open', done_date = '', done_note = '', done_document_id = NULL WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// DocumentFollowUpIDsTx lists the follow-ups a file said.
func DocumentFollowUpIDsTx(tx *sql.Tx, docID int64) ([]int64, error) {
	fs, err := followUps(tx, `document_id = ?`, docID)
	var ids []int64
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	return ids, err
}

// MoveDocumentFollowUpsTx gives a file's follow-ups to the file's owner.
func MoveDocumentFollowUpsTx(tx *sql.Tx, docID, personID int64) error {
	_, err := tx.Exec(`UPDATE followups SET person_id = ? WHERE document_id = ?`, personID, docID)
	return err
}

// Evidence is something dated that may show a follow-up done: a value, or
// a file.
type Evidence struct {
	Date       string
	Name       string // the value's name, or the file's title
	Kind       string // the file's kind ("" for a value)
	Source     string // the value's source: document, device, chat…
	DocumentID *int64
}

// EvidenceAfterTx lists a person's values and filed files dated after a
// day, oldest first.
func EvidenceAfterTx(tx *sql.Tx, personID int64, after string) ([]Evidence, error) {
	rows, err := tx.Query(`
		SELECT observed, name, '', source, document_id FROM observations WHERE person_id = ? AND substr(observed, 1, 10) > ?
		UNION ALL
		SELECT CASE WHEN doc_date != '' THEN doc_date ELSE substr(added, 1, 10) END, CASE WHEN title != '' THEN title ELSE filename END, kind, 'file', id
		  FROM documents WHERE person_id = ? AND status = 'done' AND (CASE WHEN doc_date != '' THEN doc_date ELSE substr(added, 1, 10) END) > ?
		ORDER BY 1`, personID, after, personID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var doc sql.NullInt64
		if err := rows.Scan(&e.Date, &e.Name, &e.Kind, &e.Source, &doc); err != nil {
			return nil, err
		}
		if doc.Valid {
			e.DocumentID = &doc.Int64
		}
		if len(e.Date) > 10 {
			e.Date = e.Date[:10]
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
