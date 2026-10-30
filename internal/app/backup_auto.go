package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DeGrandis/doc-ish/internal/brand"
)

// Automatic backups to a folder: typically a NAS share mounted into the
// container (/backup in docker-compose.yml). They run on a schedule even
// while the records are locked, since a backup never needs the key. The
// settings live beside the data (not in the encrypted database) for that
// reason; they hold nothing private.

type autoBackup struct {
	Dir       string `json:"dir"`
	Keep      int    `json:"keep"` // newest backups kept
	EveryHrs  int    `json:"every_hours"`
	Last      string `json:"last,omitempty"`
	LastFile  string `json:"last_file,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

var autoMu sync.Mutex

func (a *App) autoPath() string { return filepath.Join(a.cfg.DataDir, "backup.json") }

func (a *App) loadAuto() autoBackup {
	b := autoBackup{Dir: a.cfg.BackupDir, Keep: 7, EveryHrs: 24}
	if raw, err := os.ReadFile(a.autoPath()); err == nil {
		json.Unmarshal(raw, &b)
	}
	if b.Keep < 1 {
		b.Keep = 7
	}
	if b.EveryHrs < 1 {
		b.EveryHrs = 24
	}
	return b
}

func (a *App) saveAuto(b autoBackup) error {
	raw, _ := json.MarshalIndent(b, "", "  ")
	tmp := a.autoPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.autoPath())
}

// backupLoop runs due backups. It checks every ten minutes.
func (a *App) backupLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		a.backupIfDue()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) backupIfDue() {
	b := a.loadAuto()
	if b.Dir == "" || !a.hasRecords() {
		return
	}
	if last, err := time.Parse(time.RFC3339, b.Last); err == nil && time.Since(last) < time.Duration(b.EveryHrs)*time.Hour {
		return
	}
	a.runAutoBackup()
}

func (a *App) hasRecords() bool {
	_, err := os.Stat(filepath.Join(a.cfg.DataDir, "keys.json"))
	return err == nil
}

// runAutoBackup writes one backup into the folder and prunes old ones.
func (a *App) runAutoBackup() autoBackup {
	autoMu.Lock()
	defer autoMu.Unlock()
	b := a.loadAuto()
	err := func() error {
		if b.Dir == "" {
			return errors.New("no backup folder set")
		}
		if fi, err := os.Stat(b.Dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("the folder %s isn't there (is the share mounted?)", b.Dir)
		}
		name := fmt.Sprintf("%s-backup-%s.tar", brand.Code, time.Now().Format("2006-01-02-1504"))
		tmp := filepath.Join(b.Dir, name+".part")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("can't write to %s: %v", b.Dir, err)
		}
		if err := a.writeBackup(f); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, filepath.Join(b.Dir, name)); err != nil {
			return err
		}
		b.LastFile = name
		prune(b.Dir, b.Keep)
		return nil
	}()
	b.Last = time.Now().UTC().Format(time.RFC3339)
	b.LastError = ""
	if err != nil {
		b.LastError = err.Error()
		log.Printf("automatic backup: %v", err)
	}
	a.saveAuto(b)
	return b
}

// prune keeps the newest keep backups in dir; it only ever touches this
// app's own backup files.
func prune(dir string, keep int) {
	matches, _ := filepath.Glob(filepath.Join(dir, brand.Code+"-backup-*.tar"))
	sort.Strings(matches) // the names sort by date
	for len(matches) > keep {
		os.Remove(matches[0])
		matches = matches[1:]
	}
}

func (a *App) handleAutoBackup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.loadAuto())
}

func (a *App) handleSaveAutoBackup(w http.ResponseWriter, r *http.Request) {
	var req autoBackup
	if !readJSON(w, r, &req) {
		return
	}
	b := a.loadAuto()
	b.Dir = strings.TrimSpace(req.Dir)
	if b.Dir != "" && !filepath.IsAbs(b.Dir) {
		writeErr(w, 400, "give the folder's full path, like /backup")
		return
	}
	b.Keep, b.EveryHrs = max(req.Keep, 1), max(req.EveryHrs, 1)
	if err := a.saveAuto(b); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, b)
}

func (a *App) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	b := a.runAutoBackup()
	if b.LastError != "" {
		writeErr(w, 400, b.LastError)
		return
	}
	writeJSON(w, b)
}
