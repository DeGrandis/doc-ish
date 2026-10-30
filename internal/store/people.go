package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Person is one profile in the household.
type Person struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	Profile Profile `json:"profile"`
	Created string  `json:"created"`
	Updated string  `json:"updated"`
}

// Profile is what a new doctor's office asks for. Lists are free-form
// items so the assistant can fill them from an interview as easily as a
// person can by hand.
type Profile struct {
	Born             string `json:"born,omitempty"` // YYYY-MM-DD
	Sex              string `json:"sex,omitempty"`
	Height           string `json:"height,omitempty"` // as entered, e.g. 5'10" or 178 cm
	BloodType        string `json:"blood_type,omitempty"`
	Conditions       []Item `json:"conditions,omitempty"`
	Surgeries        []Item `json:"surgeries,omitempty"`
	Allergies        []Item `json:"allergies,omitempty"`
	Medications      []Item `json:"medications,omitempty"`
	FamilyHistory    []Item `json:"family_history,omitempty"`
	Vaccinations     []Item `json:"vaccinations,omitempty"`
	Providers        []Item `json:"providers,omitempty"`
	Lifestyle        string `json:"lifestyle,omitempty"` // smoking, alcohol, exercise
	EmergencyContact string `json:"emergency_contact,omitempty"`
	Insurance        string `json:"insurance,omitempty"`
	Notes            string `json:"notes,omitempty"`
}

// Item is one entry in a profile list: "Penicillin" / "hives".
type Item struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Since  string `json:"since,omitempty"`
}

func scanPerson(row interface{ Scan(...any) error }) (Person, error) {
	var p Person
	var prof string
	if err := row.Scan(&p.ID, &p.Name, &p.Color, &prof, &p.Created, &p.Updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, ErrNotFound
		}
		return p, err
	}
	json.Unmarshal([]byte(prof), &p.Profile)
	return p, nil
}

const personCols = `id, name, color, profile, created, updated`

func (s *Store) People() ([]Person, error) {
	rows, err := s.db.Query(`SELECT ` + personCols + ` FROM people ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Person(id int64) (Person, error) {
	return scanPerson(s.db.QueryRow(`SELECT `+personCols+` FROM people WHERE id = ?`, id))
}

func (s *Store) AddPerson(name, color string) (Person, error) {
	t := now()
	res, err := s.db.Exec(`INSERT INTO people(name, color, profile, created, updated) VALUES(?, ?, '{}', ?, ?)`, name, color, t, t)
	if err != nil {
		return Person{}, err
	}
	id, _ := res.LastInsertId()
	return s.Person(id)
}

func (s *Store) UpdatePerson(p Person) error {
	prof, err := json.Marshal(p.Profile)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE people SET name = ?, color = ?, profile = ?, updated = ? WHERE id = ?`,
		p.Name, p.Color, string(prof), now(), p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePerson removes the profile and everything on it, and returns the
// blob names of its files so the caller can delete them too.
func (s *Store) DeletePerson(id int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT blob FROM documents WHERE person_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var blobs []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		blobs = append(blobs, b)
	}
	rows.Close()
	_, err = s.db.Exec(`DELETE FROM people WHERE id = ?`, id)
	return blobs, err
}
