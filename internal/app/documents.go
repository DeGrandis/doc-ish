package app

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DeGrandis/mymedical/internal/blobs"
	"github.com/DeGrandis/mymedical/internal/imaging"
	"github.com/DeGrandis/mymedical/internal/store"
)

func (a *App) handleDocuments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		ds, err := st.Documents(id, r.URL.Query().Get("q"), limit)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, ds)
	})
}

type uploadResult struct {
	Filename  string          `json:"filename"`
	Document  *store.Document `json:"document,omitempty"`
	Duplicate bool            `json:"duplicate,omitempty"`
	Images    int             `json:"images,omitempty"` // for an imaging study: images added to it
	Error     string          `json:"error,omitempty"`
}

// handleUpload stores each file in a multipart upload on the profile in
// the URL. Files are streamed straight into encrypted storage, so size is
// limited only by the disk.
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "send the files as a form upload")
		return
	}
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		if _, err := st.Person(pid); err != nil {
			storeErr(w, err)
			return
		}
		results := []uploadResult{}
		studies := map[int64]int{} // study document -> its place in results
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeErr(w, 400, "upload interrupted: "+err.Error())
				return
			}
			name := filepath.Base(part.FileName())
			if part.FormName() != "file" || name == "." || name == "" {
				part.Close()
				continue
			}
			res := uploadResult{Filename: name}
			br := bufio.NewReaderSize(part, 4096)
			head, _ := br.Peek(512)
			kind := sniff(name, head)

			// DICOM images join their study; a zip (a hospital CD) is unpacked.
			if kind == "application/dicom" {
				data, err := imaging.ReadAll(br)
				part.Close()
				if err == nil {
					docID, added, derr := a.addDicom(st, files, pid, data, name)
					if derr == nil {
						if i, ok := studies[docID]; ok {
							if added {
								results[i].Images++
							}
							continue
						}
						d, _ := st.Document(docID)
						res.Document, res.Images, res.Duplicate = &d, b2i(added), !added
						studies[docID] = len(results)
						results = append(results, res)
						continue
					}
				}
				res.Error = "this DICOM file couldn't be read"
				results = append(results, res)
				continue
			}
			if strings.EqualFold(filepath.Ext(name), ".zip") {
				path, err := spool(br, filepath.Join(a.cfg.DataDir, "tmp"))
				part.Close()
				if err != nil {
					res.Error = "couldn't take the zip in"
					results = append(results, res)
					continue
				}
				got, err := a.fromZip(st, files, pid, path, name)
				os.Remove(path)
				if err != nil {
					res.Error = err.Error()
					results = append(results, res)
					continue
				}
				results = append(results, got...)
				continue
			}

			blob, size, sum, err := files.Put(br)
			part.Close()
			if err != nil {
				log.Printf("upload %s: %v", name, err)
				res.Error = "couldn't store it"
				results = append(results, res)
				continue
			}
			if size == 0 {
				files.Delete(blob)
				res.Error = "the file is empty"
				results = append(results, res)
				continue
			}
			d, err := st.AddDocument(store.Document{PersonID: pid, Blob: blob, Filename: name, Mime: kind, Size: size, SHA256: sum})
			if errors.Is(err, store.ErrDuplicate) {
				files.Delete(blob)
				res.Duplicate = true
				res.Document = &d
			} else if err != nil {
				files.Delete(blob)
				log.Printf("upload %s: %v", name, err)
				res.Error = "couldn't save it"
			} else {
				res.Document = &d
			}
			results = append(results, res)
		}
		var added []string
		for _, res := range results {
			if res.Document != nil && !res.Duplicate {
				added = append(added, res.Filename)
			}
		}
		if len(added) > 0 {
			what := added[0]
			if len(added) > 1 {
				what = fmt.Sprintf("%d files", len(added))
			}
			st.Log(pid, "you", "Added "+what, nil)
			a.wakeReader()
		}
		if len(studies) > 0 {
			linkReports(st, pid)
		}
		writeJSON(w, results)
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sniff works out the media type from the content first, then the name.
func sniff(name string, head []byte) string {
	if len(head) >= 132 && string(head[128:132]) == "DICM" {
		return "application/dicom"
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".dcm", ".dicom":
		return "application/dicom"
	case ".csv":
		return "text/csv"
	case ".tsv", ".tab":
		return "text/tab-separated-values"
	case ".json":
		return "application/json"
	case ".xml", ".ccd", ".ccda":
		return "application/xml"
	case ".heic":
		return "image/heic"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	}
	t := http.DetectContentType(head)
	if t == "application/octet-stream" || strings.HasPrefix(t, "text/plain") {
		if bt := mime.TypeByExtension(ext); bt != "" {
			return bt
		}
	}
	if bytes.HasPrefix(head, []byte("%PDF")) {
		return "application/pdf"
	}
	return t
}

func (a *App) handleDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		d, err := st.Document(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, viewOf(st, d))
	})
}

func (a *App) handleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	var req struct {
		PersonID *int64  `json:"person_id"`
		Kind     *string `json:"kind"`
		Title    *string `json:"title"`
		DocDate  *string `json:"doc_date"`
		Summary  *string `json:"summary"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	a.withStore(w, func(st *store.Store, _ *blobs.Store) {
		d, err := st.Document(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		prev := snapshot(d)
		summary := "Edited " + d.Filename
		if req.PersonID != nil && *req.PersonID != d.PersonID {
			to, err := st.Person(*req.PersonID)
			if err != nil {
				storeErr(w, err)
				return
			}
			d.PersonID = to.ID
			summary = "Moved " + d.Filename + " to " + to.Name
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = strings.TrimSpace(*v)
			}
		}
		set(&d.Kind, req.Kind)
		set(&d.Title, req.Title)
		set(&d.DocDate, req.DocDate)
		set(&d.Summary, req.Summary)
		err = st.Tx(func(tx *sql.Tx) error {
			if err := store.UpdateDocumentTx(tx, d); err != nil {
				return err
			}
			// Values read from the file, and an imaging study, belong to
			// whoever the file does.
			if err := store.MoveDocumentObservationsTx(tx, d.ID, d.PersonID); err != nil {
				return err
			}
			if err := store.MoveStudyTx(tx, d.ID, d.PersonID); err != nil {
				return err
			}
			// Logged on the profile it came from, where it can be undone.
			return store.LogDocTx(tx, prev.PersonID, d.ID, "you", summary, undoOp{Op: "document", DocumentID: d.ID, Prev: &prev})
		})
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, viewOf(st, d))
	})
}

func (a *App) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		d, err := st.Document(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if d.Mime == "application/dicom" {
			deleteStudyBlobs(st, files, id) // before the study's rows go with the document
		}
		if err := st.DeleteDocument(id); err != nil {
			storeErr(w, err)
			return
		}
		files.Delete(d.Blob)
		st.LogDoc(d.PersonID, d.ID, "you", "Deleted "+d.Filename, nil)
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// inlineTypes are the only types shown in the browser; anything else
// downloads, so an uploaded HTML or SVG file can never run as this site.
var inlineTypes = map[string]bool{
	"application/pdf": true, "image/png": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true, "text/plain": true, "text/csv": true,
}

func (a *App) handleDocumentFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad id")
		return
	}
	a.withStore(w, func(st *store.Store, files *blobs.Store) {
		d, err := st.Document(id)
		if err != nil {
			storeErr(w, err)
			return
		}
		if d.Mime == "application/dicom" && r.URL.Query().Get("download") != "" {
			if _, err := st.StudyOf(id); err == nil {
				serveStudyZip(w, st, files, d)
				return
			}
		}
		rc, err := files.Open(d.Blob)
		if err != nil {
			log.Printf("open file %d: %v", id, err)
			writeErr(w, 500, "couldn't open the stored file")
			return
		}
		defer rc.Close()
		base := strings.SplitN(d.Mime, ";", 2)[0]
		disp := "attachment"
		ctype := "application/octet-stream"
		if inlineTypes[base] && r.URL.Query().Get("download") == "" {
			disp = "inline"
			ctype = base
			if strings.HasPrefix(base, "text/") {
				ctype = "text/plain; charset=utf-8"
			}
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": d.Filename}))
		h.Set("Content-Length", strconv.FormatInt(d.Size, 10))
		h.Set("Cache-Control", "private, no-store")
		// Browsers won't show a PDF under a sandbox policy; everything
		// else gets one.
		if ctype != "application/pdf" {
			h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
		}
		io.Copy(w, rc)
	})
}
