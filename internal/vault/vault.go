// Package vault turns the password (or the recovery key) into the data
// key that encrypts the database and every stored file. The data key is
// random and never written down in the clear: keys.json holds it twice,
// once wrapped by a key derived from the password and once by one derived
// from the recovery key. Forgetting both means the data is gone; that is
// the point.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const fileName = "keys.json"

// ErrWrong is returned for a wrong password or recovery key.
var ErrWrong = errors.New("wrong password")

type kdf struct {
	Salt    []byte `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"` // KiB
	Threads uint8  `json:"threads"`
}

type wrapped struct {
	KDF kdf    `json:"kdf"`
	Box []byte `json:"box"` // nonce || AES-256-GCM(data key)
}

type file struct {
	Version  int     `json:"version"`
	Password wrapped `json:"password"`
	Recovery wrapped `json:"recovery"`
}

// Exists reports whether setup has happened in dir.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, fileName))
	return err == nil
}

// Create makes a new data key, wraps it with the password and a fresh
// recovery key, and returns both. The recovery key is shown to the user
// once and never stored.
func Create(dir, password string) (dataKey []byte, recovery string, err error) {
	if Exists(dir) {
		return nil, "", errors.New("already set up")
	}
	dataKey = random(32)
	recovery = newRecovery()
	pw, err := wrap(dataKey, password)
	if err != nil {
		return nil, "", err
	}
	rc, err := wrap(dataKey, normalizeRecovery(recovery))
	if err != nil {
		return nil, "", err
	}
	if err := save(dir, file{Version: 1, Password: pw, Recovery: rc}); err != nil {
		return nil, "", err
	}
	return dataKey, recovery, nil
}

// Unlock returns the data key for the password.
func Unlock(dir, password string) ([]byte, error) {
	f, err := load(dir)
	if err != nil {
		return nil, err
	}
	return unwrap(f.Password, password)
}

// Recover unlocks with the recovery key and sets a new password.
func Recover(dir, recovery, newPassword string) ([]byte, error) {
	f, err := load(dir)
	if err != nil {
		return nil, err
	}
	key, err := unwrap(f.Recovery, normalizeRecovery(recovery))
	if err != nil {
		return nil, errors.New("that recovery key doesn't match")
	}
	if f.Password, err = wrap(key, newPassword); err != nil {
		return nil, err
	}
	return key, save(dir, f)
}

// ChangePassword rewraps the data key under a new password.
func ChangePassword(dir, oldPassword, newPassword string) error {
	f, err := load(dir)
	if err != nil {
		return err
	}
	key, err := unwrap(f.Password, oldPassword)
	if err != nil {
		return err
	}
	if f.Password, err = wrap(key, newPassword); err != nil {
		return err
	}
	return save(dir, f)
}

func wrap(dataKey []byte, secret string) (wrapped, error) {
	k := kdf{Salt: random(16), Time: 3, Memory: 64 * 1024, Threads: 4}
	gcm, err := aead(derive(k, secret))
	if err != nil {
		return wrapped{}, err
	}
	nonce := random(gcm.NonceSize())
	return wrapped{KDF: k, Box: gcm.Seal(nonce, nonce, dataKey, nil)}, nil
}

func unwrap(w wrapped, secret string) ([]byte, error) {
	gcm, err := aead(derive(w.KDF, secret))
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(w.Box) < n {
		return nil, errors.New("keys.json is damaged")
	}
	key, err := gcm.Open(nil, w.Box[:n], w.Box[n:], nil)
	if err != nil {
		return nil, ErrWrong
	}
	return key, nil
}

func derive(k kdf, secret string) []byte {
	return argon2.IDKey([]byte(secret), k.Salt, k.Time, k.Memory, k.Threads, 32)
}

func aead(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func load(dir string) (file, error) {
	var f file
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("keys.json is damaged: %w", err)
	}
	return f, nil
}

func save(dir string, f file) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, fileName))
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// newRecovery is 160 random bits as eight groups of four letters and
// digits, easy to write on paper.
func newRecovery() string {
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random(20))
	var groups []string
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, "-")
}

// normalizeRecovery forgives case, spaces and dashes in what was typed.
func normalizeRecovery(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' || r == '\n' {
			return -1
		}
		return r
	}, s)
}
