package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Activity is one change, by a person, the file reader or the assistant,
// with what it takes to undo it.
type Activity struct {
	ID         int64           `json:"id"`
	PersonID   *int64          `json:"person_id,omitempty"`
	DocumentID *int64          `json:"document_id,omitempty"`
	At         string          `json:"at"`
	Actor      string          `json:"actor"` // you, reader, assistant
	Summary    string          `json:"summary"`
	Undo       json.RawMessage `json:"-"`
	CanUndo    bool            `json:"can_undo"`
	Undone     string          `json:"undone,omitempty"`
}

// LogTx records a change. undo may be nil for changes that can't be undone.
func LogTx(tx *sql.Tx, personID int64, actor, summary string, undo any) error {
	return LogDocTx(tx, personID, 0, actor, summary, undo)
}

// LogDocTx records a change to one file. Only the latest change to a file
// can be undone: undoing an older one would put back a state that later
// changes have built on.
func LogDocTx(tx *sql.Tx, personID, docID int64, actor, summary string, undo any) error {
	_, err := LogIDTx(tx, personID, docID, actor, summary, undo)
	return err
}

// LogIDTx records a change and returns its id, for an Undo button.
func LogIDTx(tx *sql.Tx, personID, docID int64, actor, summary string, undo any) (int64, error) {
	var u []byte
	if undo != nil {
		var err error
		if u, err = json.Marshal(undo); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`INSERT INTO activity(person_id, document_id, at, actor, summary, undo) VALUES(?, ?, ?, ?, ?, ?)`,
		nullID(personID), nullID(docID), now(), actor, summary, string(u))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Log records a change outside a transaction.
func (s *Store) Log(personID int64, actor, summary string, undo any) error {
	return s.Tx(func(tx *sql.Tx) error { return LogTx(tx, personID, actor, summary, undo) })
}

// LogDoc records a change to one file outside a transaction.
func (s *Store) LogDoc(personID, docID int64, actor, summary string, undo any) error {
	return s.Tx(func(tx *sql.Tx) error { return LogDocTx(tx, personID, docID, actor, summary, undo) })
}

// activityCols works out can-undo in SQL: there's an undo record, it
// hasn't been used, and no later change touched the same file.
const activityCols = `a.id, a.person_id, a.document_id, a.at, a.actor, a.summary, a.undo, a.undone,
	a.undo != '' AND a.undone = '' AND (a.document_id IS NULL OR NOT EXISTS (
		SELECT 1 FROM activity b WHERE b.document_id = a.document_id AND b.id > a.id AND b.undone = ''))`

// ActivityFor lists a profile's changes, newest first, plus changes that
// belong to no profile.
func (s *Store) ActivityFor(personID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+activityCols+` FROM activity a
		WHERE a.person_id = ? OR a.person_id IS NULL ORDER BY a.id DESC LIMIT ?`, personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanActivity(row interface{ Scan(...any) error }) (Activity, error) {
	var a Activity
	var undo string
	err := row.Scan(&a.ID, &a.PersonID, &a.DocumentID, &a.At, &a.Actor, &a.Summary, &undo, &a.Undone, &a.CanUndo)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.Undo = json.RawMessage(undo)
	return a, err
}

// ActivityTx reads one change inside a transaction.
func ActivityTx(tx *sql.Tx, id int64) (Activity, error) {
	return scanActivity(tx.QueryRow(`SELECT `+activityCols+` FROM activity a WHERE a.id = ?`, id))
}

// MarkUndoneTx records that a change was undone.
func MarkUndoneTx(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`UPDATE activity SET undone = ? WHERE id = ?`, now(), id)
	return err
}

// PersonTx reads a profile inside a transaction.
func PersonTx(tx *sql.Tx, id int64) (Person, error) {
	return scanPerson(tx.QueryRow(`SELECT `+personCols+` FROM people WHERE id = ?`, id))
}

// UpdatePersonTx saves a profile inside a transaction.
func UpdatePersonTx(tx *sql.Tx, p Person) error {
	prof, err := json.Marshal(p.Profile)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE people SET name = ?, color = ?, profile = ?, updated = ? WHERE id = ?`, p.Name, p.Color, string(prof), now(), p.ID)
	return err
}

// DocumentTx reads a file's record inside a transaction.
func DocumentTx(tx *sql.Tx, id int64) (Document, error) {
	d, err := scanDoc(tx.QueryRow(`SELECT `+docCols+` FROM documents WHERE id = ?`, id))
	if err != nil {
		return d, err
	}
	err = tx.QueryRow(`SELECT text FROM documents WHERE id = ?`, id).Scan(&d.Text)
	return d, err
}
