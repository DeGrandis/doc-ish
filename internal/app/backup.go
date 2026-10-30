package app

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DeGrandis/doc-ish/internal/brand"
	"github.com/DeGrandis/doc-ish/internal/vault"
)

// A backup is a tar of the data directory as it is on disk: keys.json, the
// encrypted database and the encrypted files, with a manifest. Nothing in
// it is readable without the password (or recovery key) the data had when
// the backup was made, so it can sit on a NAS or a USB stick as it is.
// The public reference data isn't included; it can be downloaded again.

const (
	backupFormat  = "docish-backup"
	backupVersion = 1
)

type manifest struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	Created string `json:"created"`
	App     string `json:"app"`
	Files   int    `json:"files"`
}

// snapshotDB copies the database while nothing can write to it, so the
// copy is consistent. The copy is as encrypted as the original.
func (a *App) snapshotDB() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	src, err := os.Open(filepath.Join(a.cfg.DataDir, "records.db"))
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp := filepath.Join(a.cfg.DataDir, "tmp")
	os.MkdirAll(tmp, 0o700)
	dst, err := os.CreateTemp(tmp, "backup-*.db")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return "", err
	}
	return dst.Name(), dst.Close()
}

// writeBackup writes the whole backup to w.
func (a *App) writeBackup(w io.Writer) error {
	db, err := a.snapshotDB()
	if err != nil {
		return fmt.Errorf("couldn't copy the database: %w", err)
	}
	defer os.Remove(db)
	var blobs []string
	filesDir := filepath.Join(a.cfg.DataDir, "files")
	filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".age") {
			blobs = append(blobs, p)
		}
		return nil
	})
	sort.Strings(blobs)

	tw := tar.NewWriter(w)
	add := func(name string, r io.Reader, size int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	addFile := func(name, path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		return add(name, f, fi.Size())
	}
	m, _ := json.MarshalIndent(manifest{Format: backupFormat, Version: backupVersion, Created: time.Now().UTC().Format(time.RFC3339),
		App: a.cfg.Version, Files: len(blobs)}, "", "  ")
	if err := add("manifest.json", strings.NewReader(string(m)), int64(len(m))); err != nil {
		return err
	}
	if err := addFile("keys.json", filepath.Join(a.cfg.DataDir, "keys.json")); err != nil {
		return err
	}
	if err := addFile("records.db", db); err != nil {
		return err
	}
	for _, p := range blobs {
		rel, _ := filepath.Rel(a.cfg.DataDir, p)
		if err := addFile(filepath.ToSlash(rel), p); errors.Is(err, fs.ErrNotExist) {
			continue // deleted while the backup ran
		} else if err != nil {
			return err
		}
	}
	return tw.Close()
}

// handleBackup downloads a backup. It asks for the password again: a
// backup is everything, and someone at an unlocked screen shouldn't walk
// off with it.
func (a *App) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := vault.Unlock(a.cfg.DataDir, req.Password); err != nil {
		writeErr(w, 403, "that isn't the password")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-backup-%s.tar"`, brand.Code, time.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	if err := a.writeBackup(w); err != nil {
		log.Printf("backup: %v", err)
	}
}

// ---------- restore ----------

// handleRestore sets up a fresh install from a backup. It is only possible
// before setup, so it can never overwrite records. The backup opens with
// the password it was made with, or with its recovery key and a new
// password.
func (a *App) handleRestore(w http.ResponseWriter, r *http.Request) {
	if vault.Exists(a.cfg.DataDir) {
		writeErr(w, 409, "this install already has records; restoring is only for a fresh install")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "send the backup as a form upload")
		return
	}
	fields := map[string]string{}
	var tarPath string
	defer func() {
		if tarPath != "" {
			os.Remove(tarPath)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, "upload interrupted")
			return
		}
		if part.FormName() == "file" {
			if tarPath, err = spool(part, filepath.Join(a.cfg.DataDir, "tmp")); err != nil {
				writeErr(w, 500, "couldn't take the backup in")
				return
			}
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(part, 4096))
		fields[part.FormName()] = string(b)
	}
	if tarPath == "" {
		writeErr(w, 400, "choose a backup file")
		return
	}
	key, err := a.restore(tarPath, fields)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.unlock(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.newSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

var restoreMu sync.Mutex

// restore checks the backup opens with what was given, then puts its
// files in place, keys.json last (so a restore that fails halfway leaves
// a fresh install, not a broken one).
func (a *App) restore(tarPath string, fields map[string]string) ([]byte, error) {
	restoreMu.Lock()
	defer restoreMu.Unlock()
	stage, err := os.MkdirTemp(a.cfg.DataDir, "restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var m manifest
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("this isn't a backup file, or it's damaged")
		}
		name := filepath.FromSlash(h.Name)
		clean := filepath.Clean(name)
		if h.Typeflag != tar.TypeReg || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			continue
		}
		if clean == "manifest.json" {
			b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			json.Unmarshal(b, &m)
			continue
		}
		if clean != "keys.json" && clean != "records.db" && !strings.HasPrefix(clean, "files"+string(filepath.Separator)) {
			continue
		}
		dst := filepath.Join(stage, clean)
		os.MkdirAll(filepath.Dir(dst), 0o700)
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return nil, errors.New("the backup file is damaged")
		}
	}
	if m.Format != backupFormat {
		return nil, fmt.Errorf("this isn't a %s backup file", brand.Name)
	}
	if m.Version > backupVersion {
		return nil, fmt.Errorf("this backup is from a newer version of %s; update the app first", brand.Name)
	}
	if _, err := os.Stat(filepath.Join(stage, "records.db")); err != nil {
		return nil, errors.New("the backup has no records in it")
	}

	var key []byte
	if rec := strings.TrimSpace(fields["recovery"]); rec != "" {
		if err := checkPassword(fields["new_password"]); err != nil {
			return nil, err
		}
		if key, err = vault.Recover(stage, rec, fields["new_password"]); err != nil {
			return nil, err
		}
	} else if key, err = vault.Unlock(stage, fields["password"]); err != nil {
		return nil, errors.New("that isn't the password this backup was made with (try its recovery key)")
	}

	// Put the records in place, then the keys.
	os.RemoveAll(filepath.Join(a.cfg.DataDir, "files"))
	if err := os.Rename(filepath.Join(stage, "files"), filepath.Join(a.cfg.DataDir, "files")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(filepath.Join(stage, "records.db"), filepath.Join(a.cfg.DataDir, "records.db")); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(stage, "keys.json"), filepath.Join(a.cfg.DataDir, "keys.json")); err != nil {
		return nil, err
	}
	return key, nil
}
