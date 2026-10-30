package blobs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ident, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir(), ident)
	if err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("Hemoglobin A1c 5.4 %\n"), 5000)
	name, size, sum, err := s.Put(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(plain)) || len(sum) != 64 {
		t.Fatalf("size %d sum %q", size, sum)
	}
	raw, _ := os.ReadFile(s.path(name))
	if bytes.Contains(raw, []byte("Hemoglobin")) {
		t.Fatal("stored file is not encrypted")
	}
	r, err := s.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, plain) {
		t.Fatal("round trip changed the bytes")
	}
	if err := s.Delete(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, name[:2], name+".age")); !os.IsNotExist(err) {
		t.Fatal("file still there")
	}
}
