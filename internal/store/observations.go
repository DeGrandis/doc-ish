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

// ObservationsIn lists every reading of one measurement (case-insensitive)
// between two days, inclusive; either may be empty. No limit: a device
// import can hold years of daily values.
func (s *Store) ObservationsIn(personID int64, name, from, to string) ([]Observation, error) {
	where := `person_id = ? AND name = ? COLLATE NOCASE`
	args := []any{personID, name}
	if from != "" {
		where += ` AND substr(observed, 1, 10) >= ?`
		args = append(args, from)
	}
	if to != "" {
		where += ` AND substr(observed, 1, 10) <= ?`
		args = append(args, to)
	}
	return s.observations(where+` ORDER BY observed, id`, args...)
}

// DocumentObservations lists the values read from one file.
func (s *Store) DocumentObservations(docID int64) ([]Observation, error) {
	return s.observations(`document_id = ? ORDER BY id`, docID)
}

// Latest returns the most recent value of one measurement, if any.
func (s *Store) Latest(personID int64, name string) (*Observation, error) {
	all, err := s.observations(`person_id = ? AND name = ? COLLATE NOCASE ORDER BY observed DESC, id DESC LIMIT 1`, personID, name)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

// Measurement is one kind of value on record, summed up.
type Measurement struct {
	Name   string
	Count  int
	First  string // when it was first observed
	Latest Observation
}

// Measurements lists every kind of value on a profile, in the order each
// was first added, with its latest reading.
func (s *Store) Measurements(personID int64) ([]Measurement, error) {
	rows, err := s.db.Query(`SELECT name, count(*), min(observed) FROM observations WHERE person_id = ?
		GROUP BY name COLLATE NOCASE ORDER BY min(id)`, personID)
	if err != nil {
		return nil, err
	}
	var out []Measurement
	for rows.Next() {
		var m Measurement
		if err := rows.Scan(&m.Name, &m.Count, &m.First); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if l, err := s.Latest(personID, out[i].Name); err == nil && l != nil {
			out[i].Latest = *l
		}
	}
	return out, nil
}

// SourceObservationsTx lists a profile's values from one source (an
// import), for skipping readings an earlier import already brought in.
func SourceObservationsTx(tx *sql.Tx, personID int64, source string) ([]Observation, error) {
	rows, err := tx.Query(`SELECT `+obsCols+` FROM observations WHERE person_id = ? AND source = ?`, personID, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
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

// AddObservationsTx adds many values with one prepared statement.
func AddObservationsTx(tx *sql.Tx, obs []Observation) error {
	stmt, err := tx.Prepare(`INSERT INTO observations(person_id, name, code, value, value_text, unit, ref_low, ref_high, flag, observed, document_id, source, note, added)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	t := now()
	for _, o := range obs {
		if _, err := stmt.Exec(o.PersonID, o.Name, o.Code, o.Value, o.ValueText, o.Unit, o.RefLow, o.RefHigh, o.Flag, o.Observed,
			o.DocumentID, o.Source, o.Note, t); err != nil {
			return err
		}
	}
	return nil
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
