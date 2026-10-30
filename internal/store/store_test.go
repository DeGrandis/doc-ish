package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	s, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.AddPerson("Zebulon Testperson", "#ff7a59")
	if err != nil {
		t.Fatal(err)
	}
	p.Profile.Allergies = []Item{{Name: "Penicillin", Detail: "hives"}}
	if err := s.UpdatePerson(p); err != nil {
		t.Fatal(err)
	}
	v := 182.4
	if _, err := s.AddObservation(Observation{PersonID: p.ID, Name: "Weight", Value: &v, Unit: "lb", Observed: "2026-10-01", Source: "chat"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Zebulon", "Penicillin", "SQLite format"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("%q readable in the database file", needle)
		}
	}

	if s2, err := Open(dir, bytes.Repeat([]byte{8}, 32)); err == nil {
		s2.Close()
		t.Fatal("opened with the wrong key")
	}

	s, err = Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Person(p.ID)
	if err != nil || len(got.Profile.Allergies) != 1 || got.Profile.Allergies[0].Name != "Penicillin" {
		t.Fatalf("reopen: %+v %v", got, err)
	}
	w, err := s.Latest(p.ID, "weight")
	if err != nil || w == nil || *w.Value != 182.4 {
		t.Fatalf("latest weight: %+v %v", w, err)
	}
}
