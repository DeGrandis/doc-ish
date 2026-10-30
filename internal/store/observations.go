package store

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
	Observed   string   `json:"observed"`
	DocumentID *int64   `json:"document_id,omitempty"`
	Source     string   `json:"source"` // profile, chat, document
	Note       string   `json:"note,omitempty"`
	Added      string   `json:"added"`
}

func (s *Store) AddObservation(o Observation) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO observations(person_id, name, code, value, value_text, unit, ref_low, ref_high, observed, document_id, source, note, added)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.PersonID, o.Name, o.Code, o.Value, o.ValueText, o.Unit, o.RefLow, o.RefHigh, o.Observed, o.DocumentID, o.Source, o.Note, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Observations lists a profile's values, oldest first; name filters to
// one measurement (case-insensitive) when not empty.
func (s *Store) Observations(personID int64, name string, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 1000
	}
	where := `person_id = ?`
	args := []any{personID}
	if name != "" {
		where += ` AND name = ? COLLATE NOCASE`
		args = append(args, name)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, person_id, name, code, value, value_text, unit, ref_low, ref_high, observed, document_id, source, note, added
		FROM observations WHERE `+where+` ORDER BY observed, id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.ID, &o.PersonID, &o.Name, &o.Code, &o.Value, &o.ValueText, &o.Unit, &o.RefLow, &o.RefHigh,
			&o.Observed, &o.DocumentID, &o.Source, &o.Note, &o.Added); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Latest returns the most recent value of one measurement, if any.
func (s *Store) Latest(personID int64, name string) (*Observation, error) {
	all, err := s.Observations(personID, name, 0)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[len(all)-1], nil
}
