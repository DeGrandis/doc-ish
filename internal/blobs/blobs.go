// Package blobs keeps the original files, each encrypted with age to an
// X25519 key that lives inside the (encrypted) database. Files are named
// by random ids, so their names say nothing about what they hold.
package blobs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

type Store struct {
	dir string
	id  *age.X25519Identity
}

// NewIdentity makes the key for a new install, as a string to keep in the
// database.
func NewIdentity() (string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Open uses dir for the files and identity (from NewIdentity) as the key.
func Open(dir, identity string) (*Store, error) {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, fmt.Errorf("file key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, id: id}, nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name[:2], name+".age")
}

// Put encrypts r into a new file and returns its name, its plain size and
// the SHA-256 of its plain bytes.
func (s *Store) Put(r io.Reader) (name string, size int64, sum string, err error) {
	b := make([]byte, 16)
	rand.Read(b)
	name = hex.EncodeToString(b)
	p := s.path(name)
	if err = os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p), "upload-*")
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	w, err := age.Encrypt(f, s.id.Recipient())
	if err != nil {
		return
	}
	h := sha256.New()
	size, err = io.Copy(w, io.TeeReader(r, h))
	if err != nil {
		return
	}
	if err = w.Close(); err != nil {
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	if err = os.Rename(f.Name(), p); err != nil {
		return
	}
	return name, size, hex.EncodeToString(h.Sum(nil)), nil
}

// Open returns the plain bytes of a stored file.
func (s *Store) Open(name string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(name))
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(f, s.id)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, f}, nil
}

// Delete removes a stored file; a missing one is not an error.
func (s *Store) Delete(name string) error {
	err := os.Remove(s.path(name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
