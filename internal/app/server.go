// Package app is the HTTP side: setup and sign-in, profiles, files, the
// assistant, settings, and the embedded UI.
package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/brand"
	"github.com/DeGrandis/mymedical/internal/reference"
	"github.com/DeGrandis/mymedical/internal/store"
	"github.com/DeGrandis/mymedical/internal/vault"
)

// Config is what main passes in from flags and the environment.
type Config struct {
	DataDir   string
	Version   string
	Password  string // MYMEDICAL_PASSWORD: unlock at start
	OllamaURL string // MYMEDICAL_OLLAMA_URL: the default until Settings has one
	Web       fs.FS
}

type App struct {
	cfg Config

	mu    sync.RWMutex // guards st and files; held for writing only to lock or unlock
	st    *store.Store
	files *blobs.Store

	sessMu   sync.Mutex
	sessions map[string]time.Time // token -> last use
	fails    map[string]failure   // remote IP -> wrong passwords

	build    string         // fingerprint of the UI files, so an open page can tell it's out of date
	ref      *reference.Ref // public reference data; not personal, so not locked
	job      refJob         // the reference download running, if any
	ctx      context.Context
	wake     chan struct{} // nudges the file reader
	stop     context.CancelFunc
	readerMu sync.Mutex
	reader   readerState
}

type failure struct {
	n     int
	until time.Time
}

const sessionIdle = 30 * 24 * time.Hour

func New(cfg Config) (*App, error) {
	a := &App{cfg: cfg, sessions: map[string]time.Time{}, fails: map[string]failure{}, wake: make(chan struct{}, 1), build: uiBuild(cfg.Web)}
	ctx, stop := context.WithCancel(context.Background())
	a.ctx, a.stop = ctx, stop
	if ref, err := reference.Open(cfg.DataDir); err != nil {
		log.Printf("reference data: %v", err) // the app works without it
	} else {
		a.ref = ref
	}
	go a.readLoop(ctx)
	if cfg.Password != "" && vault.Exists(cfg.DataDir) {
		key, err := vault.Unlock(cfg.DataDir, cfg.Password)
		if err != nil {
			return nil, errors.New("MYMEDICAL_PASSWORD doesn't unlock the data in " + cfg.DataDir)
		}
		if err := a.unlock(key); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// unlock opens the database and the file store with the data key.
func (a *App) unlock(key []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st != nil {
		return nil
	}
	st, err := store.Open(a.cfg.DataDir, key)
	if err != nil {
		return err
	}
	ident := st.Setting("file_key")
	if ident == "" {
		if ident, err = blobs.NewIdentity(); err != nil {
			st.Close()
			return err
		}
		if err := st.SetSetting("file_key", ident); err != nil {
			st.Close()
			return err
		}
	}
	files, err := blobs.Open(filepath.Join(a.cfg.DataDir, "files"), ident)
	if err != nil {
		st.Close()
		return err
	}
	a.st, a.files = st, files
	st.ResetReading() // a file left half-read when the app stopped goes back in line
	a.wakeReader()
	return nil
}

// lock closes the database and forgets the key.
func (a *App) lock() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st != nil {
		a.st.Close()
	}
	a.st, a.files = nil, nil
}

func (a *App) unlocked() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.st != nil
}

// Close stops the reader and locks the app on shutdown.
func (a *App) Close() {
	a.stop()
	a.lock()
	if a.ref != nil {
		a.ref.Close()
	}
}

// Handler is the whole HTTP surface.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": a.cfg.Version})
	})
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/recover", a.handleRecover)
	mux.HandleFunc("POST /api/logout", a.handleLogout)

	auth := func(pattern string, h func(http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, a.requireAuth(h))
	}
	auth("POST /api/lock", a.handleLock)
	auth("POST /api/password", a.handlePassword)

	auth("GET /api/people", a.handlePeople)
	auth("POST /api/people", a.handleAddPerson)
	auth("GET /api/people/{id}", a.handlePerson)
	auth("PUT /api/people/{id}", a.handleUpdatePerson)
	auth("DELETE /api/people/{id}", a.handleDeletePerson)
	auth("GET /api/people/{id}/observations", a.handleObservations)
	auth("POST /api/people/{id}/observations", a.handleAddObservation)

	auth("GET /api/people/{id}/documents", a.handleDocuments)
	auth("POST /api/people/{id}/documents", a.handleUpload)
	auth("GET /api/documents/{id}", a.handleDocument)
	auth("PATCH /api/documents/{id}", a.handleUpdateDocument)
	auth("DELETE /api/documents/{id}", a.handleDeleteDocument)
	auth("GET /api/documents/{id}/file", a.handleDocumentFile)
	auth("POST /api/documents/{id}/accept", a.handleAccept)
	auth("POST /api/documents/{id}/read", a.handleReread)
	auth("GET /api/people/{id}/review", a.handleReview)
	auth("GET /api/people/{id}/activity", a.handleActivity)
	auth("POST /api/activity/{id}/undo", a.handleUndo)
	auth("GET /api/status", a.handleStatus)
	auth("GET /api/recipes", a.handleRecipes)
	auth("POST /api/recipes/{id}/approve", a.handleApproveRecipe)
	auth("POST /api/recipes/{id}/reject", a.handleRejectRecipe)
	auth("DELETE /api/recipes/{id}", a.handleDeleteRecipe)
	auth("POST /api/documents/{id}/recipe", a.handleRewriteRecipe)
	auth("GET /api/reference", a.handleReference)
	auth("POST /api/reference/{source}/download", a.handleReferenceDownload)
	auth("POST /api/reference/loinc", a.handleLOINC)

	auth("GET /api/people/{id}/chats", a.handleChats)
	auth("GET /api/chats/{id}", a.handleChatMessages)
	auth("DELETE /api/chats/{id}", a.handleDeleteChat)
	auth("POST /api/people/{id}/chat", a.handleChat)

	auth("GET /api/settings", a.handleSettings)
	auth("PUT /api/settings", a.handleSaveSettings)
	auth("GET /api/models", a.handleModels)

	static := http.FileServer(http.FS(a.cfg.Web))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			a.serveIndex(w)
			return
		}
		w.Header().Set("Cache-Control", "no-cache") // always check, so a reload after an upgrade gets the new files
		static.ServeHTTP(w, r)
	})
	return securityHeaders(sameOrigin(mux))
}

// uiBuild fingerprints the UI files. A page loaded before an upgrade
// keeps running its old script; comparing this tells it to reload.
func uiBuild(web fs.FS) string {
	h := sha256.New()
	fs.WalkDir(web, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".css")) {
			b, _ := fs.ReadFile(web, p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// serveIndex fills in the product name and version.
func (a *App) serveIndex(w http.ResponseWriter) {
	b, err := fs.ReadFile(a.cfg.Web, "index.html")
	if err != nil {
		http.Error(w, "missing UI", 500)
		return
	}
	b = bytes.ReplaceAll(b, []byte("__BRAND__"), []byte(brand.Name))
	b = bytes.ReplaceAll(b, []byte("__VERSION__"), []byte(a.cfg.Version))
	b = bytes.ReplaceAll(b, []byte("__BUILD__"), []byte(a.build))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "SAMEORIGIN")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'self' 'unsafe-inline'; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'self'")
		h.ServeHTTP(w, r)
	})
}

// sameOrigin refuses state-changing requests sent from another site.
func sameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

func (a *App) cookieName() string { return brand.Code + "_session" }

func (a *App) newSession(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	a.sessMu.Lock()
	a.sessions[tok] = time.Now()
	a.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(), Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(sessionIdle / time.Second),
	})
}

func (a *App) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return "", false
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	t, ok := a.sessions[c.Value]
	if !ok || time.Since(t) > sessionIdle {
		delete(a.sessions, c.Value)
		return "", false
	}
	a.sessions[c.Value] = time.Now()
	return c.Value, true
}

func (a *App) dropSessions(keep string) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	for t := range a.sessions {
		if t != keep {
			delete(a.sessions, t)
		}
	}
}

func (a *App) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.session(r); !ok || !a.unlocked() {
			writeErr(w, http.StatusUnauthorized, "sign in first")
			return
		}
		h(w, r)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttled reports whether this address must wait after wrong passwords.
func (a *App) throttled(r *http.Request) (time.Duration, bool) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	f := a.fails[clientIP(r)]
	if d := time.Until(f.until); d > 0 {
		return d, true
	}
	return 0, false
}

func (a *App) noteFailure(r *http.Request) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	ip := clientIP(r)
	f := a.fails[ip]
	f.n++
	if f.n >= 5 {
		f.until = time.Now().Add(time.Duration(1<<min(f.n-5, 6)) * time.Minute)
	}
	a.fails[ip] = f
}

func (a *App) clearFailures(r *http.Request) {
	a.sessMu.Lock()
	delete(a.fails, clientIP(r))
	a.sessMu.Unlock()
}

// ---------- setup and sign-in ----------

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	_, authed := a.session(r)
	writeJSON(w, map[string]any{
		"name":     brand.Name,
		"version":  a.cfg.Version,
		"setup":    vault.Exists(a.cfg.DataDir),
		"unlocked": a.unlocked(),
		"authed":   authed && a.unlocked(),
	})
}

func checkPassword(pw string) error {
	if len([]rune(pw)) < 8 {
		return errors.New("use at least 8 characters")
	}
	return nil
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if vault.Exists(a.cfg.DataDir) {
		writeErr(w, http.StatusConflict, "already set up; sign in instead")
		return
	}
	if err := checkPassword(req.Password); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, 400, "enter a name for the first profile")
		return
	}
	key, recovery, err := vault.Create(a.cfg.DataDir, req.Password)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := a.unlock(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.mu.RLock()
	p, err := a.st.AddPerson(name, palette[0])
	a.mu.RUnlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.newSession(w, r)
	writeJSON(w, map[string]any{"recovery": recovery, "person": p})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if d, wait := a.throttled(r); wait {
		writeErr(w, http.StatusTooManyRequests, "too many wrong passwords; try again in "+d.Round(time.Second).String())
		return
	}
	key, err := vault.Unlock(a.cfg.DataDir, req.Password)
	if err != nil {
		if errors.Is(err, vault.ErrWrong) {
			a.noteFailure(r)
			writeErr(w, http.StatusUnauthorized, "wrong password")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if err := a.unlock(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.clearFailures(r)
	a.newSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleRecover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Recovery string `json:"recovery"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if d, wait := a.throttled(r); wait {
		writeErr(w, http.StatusTooManyRequests, "too many tries; try again in "+d.Round(time.Second).String())
		return
	}
	if err := checkPassword(req.Password); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	key, err := vault.Recover(a.cfg.DataDir, req.Recovery, req.Password)
	if err != nil {
		a.noteFailure(r)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := a.unlock(key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.clearFailures(r)
	a.dropSessions("")
	a.newSession(w, r)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok, ok := a.session(r); ok {
		a.sessMu.Lock()
		delete(a.sessions, tok)
		a.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

// handleLock signs everyone out and forgets the key until the next sign-in.
func (a *App) handleLock(w http.ResponseWriter, r *http.Request) {
	a.dropSessions("")
	a.lock()
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkPassword(req.New); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := vault.ChangePassword(a.cfg.DataDir, req.Old, req.New); err != nil {
		if errors.Is(err, vault.ErrWrong) {
			writeErr(w, 400, "the current password is wrong")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	tok, _ := a.session(r)
	a.dropSessions(tok)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- helpers ----------

// withStore runs f with the open store, or answers 401 if it was locked
// meanwhile.
func (a *App) withStore(w http.ResponseWriter, f func(st *store.Store, files *blobs.Store)) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.st == nil {
		writeErr(w, http.StatusUnauthorized, "locked; sign in again")
		return
	}
	f(a.st, a.files)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, "bad request: "+err.Error())
		return false
	}
	return true
}

func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "not found")
		return
	}
	log.Printf("store: %v", err)
	writeErr(w, 500, "something went wrong reading or saving")
}
