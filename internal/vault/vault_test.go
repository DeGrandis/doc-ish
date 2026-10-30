package vault

import (
	"bytes"
	"strings"
	"testing"
)

func TestCreateUnlockRecover(t *testing.T) {
	dir := t.TempDir()
	key, rec, err := Create(dir, "first password")
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 || len(strings.Split(rec, "-")) != 8 {
		t.Fatalf("key %d bytes, recovery %q", len(key), rec)
	}
	if _, _, err := Create(dir, "again"); err == nil {
		t.Fatal("second Create should fail")
	}
	got, err := Unlock(dir, "first password")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := Unlock(dir, "nope"); err != ErrWrong {
		t.Fatalf("wrong password: %v", err)
	}
	// Typed sloppily: lower case, spaces instead of dashes.
	sloppy := strings.ToLower(strings.ReplaceAll(rec, "-", " "))
	got, err = Recover(dir, sloppy, "second password")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("recover: %v", err)
	}
	if _, err := Unlock(dir, "first password"); err != ErrWrong {
		t.Fatal("old password still works")
	}
	if err := ChangePassword(dir, "second password", "third"); err != nil {
		t.Fatal(err)
	}
	if got, err = Unlock(dir, "third"); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("after change: %v", err)
	}
}
