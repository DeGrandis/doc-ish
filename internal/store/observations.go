package store

import (
	"database/sql"
	"strings"
)

// Observation is one measured or reported value at a point in time.
type Observation struct {
	ID         int64    `json:"id"`
	PersonID   int64    `json:"person_id"`
	Name       string   `json:"name"`
	Code       string   `json:"code,omitempty"` // LOINC, when known
	Value      *float64 `json:"value,omitempty"`
	ValueText  string   `json:"value_text,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	RefLow     *float64 `json:"ref_low,omitempty"`
	RefHigh    *float64 `json:"ref_high,omitempty"`
	Flag       string   `json:"flag,omitempty"`
	Observed   string   `json:"observed"`
	DocumentID *int64   `json:"document_id,omitempty"`
	Source     string   `json:"source"` // profile, chat, document
	Note       string   `json:"note,omitempty"`
	Added      string   `json:"added"`
}

// OutOfRange reports whether the value is outside its reference range or
// flagged as abnormal.
func (o Observation) OutOfRange() bool {
	if f := strings.ToUpper(strings.TrimSpace(o.Flag)); f != "" && f != "N" && f != "NORMAL" {
		return true
	}
	if o.Value == nil {
		return false
	}
	return (o.RefLow != nil && *o.Value < *o.RefLow) || (o.RefHigh != nil && *o.Value > *o.RefHigh)
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
}

func (s *Store) AddObservation(o Observation) (int64, error) {
	return addObservation(s.db, o)
}

// AddObservationTx adds a value inside a transaction.
func AddObservationTx(tx *sql.Tx, o Observation) (int64, error) {
	return addObservation(tx, o)
}

func addObservation(db execer, o Observation) (int64, error) {
	res, err := db.Exec(`INSERT INTO observations(person_id, name, code, value, value_text, unit, ref_low, ref_high, flag, observed, document_id, source, note, added)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.PersonID, o.Name, o.Code, o.Value, o.ValueText, o.Unit, o.RefLow, o.RefHigh, o.Flag, o.Observed, o.DocumentID, o.Source, o.Note, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const obsCols = `id, person_id, name, code, value, value_text, unit, ref_low, ref_high, flag, observed, document_id, source, note, added`

func (s *Store) observations(where string, args ...any) ([]Observation, error) {
	rows, err := s.db.Query(`SELECT `+obsCols+` FROM observations WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.PersonID, &o.Name, &o.Code, &o.Value, &o.ValueText, &o.Unit, &o.RefLow, &o.RefHigh,
			&o.Flag, &o.Observed, &o.DocumentID, &o.Source, &o.Note, &o.Added); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Observations lists a profile's values, oldest first; name filters to
// one measurement (case-insensitive) when not empty.
func (s *Store) Observations(personID int64, name string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 1000
	}
	if name != "" {
		return s.observations(`person_id = ? AND name = ? COLLATE NOCASE ORDER BY observed, id LIMIT ?`, personID, name, limit)
	}
	return s.observations(`person_id = ? ORDER BY observed, id LIMIT ?`, personID, limit)
}

// DocumentObservations lists the values read from one file.
func (s *Store) DocumentObservations(docID int64) ([]Observation, error) {
	return s.observations(`document_id = ? ORDER BY id`, docID)
}

// Latest returns the most recent value of one measurement, if any.
func (s *Store) Latest(personID int64, name string) (*Observation, error) {
	all, err := s.Observations(personID, name, 0)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[len(all)-1], nil
}

// DeleteObservationsTx removes values by id.
func DeleteObservationsTx(tx *sql.Tx, ids []int64) error {
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM observations WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// MoveDocumentObservationsTx gives the values read from a file to the
// profile the file now belongs to.
func MoveDocumentObservationsTx(tx *sql.Tx, docID, personID int64) error {
	_, err := tx.Exec(`UPDATE observations SET person_id = ? WHERE document_id = ?`, personID, docID)
	return err
}

// DocumentObservationIDsTx lists the ids of values read from one file.
func DocumentObservationIDsTx(tx *sql.Tx, docID int64) ([]int64, error) {
	rows, err := tx.Query(`SELECT id FROM observations WHERE document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
