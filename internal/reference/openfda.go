package reference

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The sections of a drug label kept, each cut to a length that's useful
// to read and keeps the database a reasonable size.
const sectionLimit = 8000

type label struct {
	EffectiveTime     string   `json:"effective_time"`
	Indications       []string `json:"indications_and_usage"`
	Boxed             []string `json:"boxed_warning"`
	Contraindications []string `json:"contraindications"`
	Warnings          []string `json:"warnings"`
	WarningsCautions  []string `json:"warnings_and_cautions"`
	Interactions      []string `json:"drug_interactions"`
	OpenFDA           struct {
		Generic     []string `json:"generic_name"`
		Brand       []string `json:"brand_name"`
		ProductType []string `json:"product_type"`
	} `json:"openfda"`
}

func section(parts []string) string {
	s := strings.TrimSpace(strings.Join(parts, "\n"))
	if len(s) > sectionLimit {
		s = strings.ToValidUTF8(s[:sectionLimit], "") + " […]"
	}
	return s
}

// FetchLabels downloads every openFDA drug label (about 1.8 GB in a dozen
// parts) and keeps, for each generic name, the most recent label's key
// sections. Only one part is on disk at a time.
func (r *Ref) FetchLabels(ctx context.Context, c *http.Client, tmpDir string, progress Progress) error {
	progress("Finding the label files", 0, 0)
	b, err := get(ctx, c, "https://api.fda.gov/download.json", 64<<20)
	if err != nil {
		return err
	}
	var idx struct {
		Results struct {
			Drug struct {
				Label struct {
					ExportDate string `json:"export_date"`
					Partitions []struct {
						File   string    `json:"file"`
						SizeMB flexFloat `json:"size_mb"`
					} `json:"partitions"`
				} `json:"label"`
			} `json:"drug"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return fmt.Errorf("openFDA's file list: %w", err)
	}
	parts := idx.Results.Drug.Label.Partitions
	if len(parts) == 0 {
		return errors.New("openFDA lists no drug label files")
	}

	var all int64
	for _, p := range parts {
		all += int64(float64(p.SizeMB) * (1 << 20))
	}
	kept := map[string]keptLabel{}
	var before int64
	err = r.tx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM labels`, `INSERT INTO labels_fts(labels_fts) VALUES('delete-all')`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		for i, p := range parts {
			stage := fmt.Sprintf("Downloading drug labels (part %d of %d)", i+1, len(parts))
			path, err := download(ctx, c, p.File, tmpDir, stage, func(s string, done, _ int64) { progress(s, before+done, all) })
			if err != nil {
				return err
			}
			before += int64(float64(p.SizeMB) * (1 << 20))
			progress(fmt.Sprintf("Reading drug labels (part %d of %d)", i+1, len(parts)), before, all)
			err = readLabelPart(path, func(l label) error { return keepLabel(tx, l, kept) })
			os.Remove(path)
			if err != nil {
				return err
			}
		}
		// Index what was kept.
		if _, err := tx.Exec(`INSERT INTO labels_fts(rowid, generic, brands) SELECT id, generic, brands FROM labels`); err != nil {
			return err
		}
		return setSource(tx, Labels, idx.Results.Drug.Label.ExportDate, len(kept))
	})
	return err
}

// readLabelPart streams the labels out of one part's zip.
func readLabelPart(zipPath string, each func(label) error) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = streamResults(rc, each)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// streamResults decodes {"meta": …, "results": [label, …]} one label at a
// time, so a part never has to fit in memory. Only the top-level
// "results" counts: meta has a "results" of its own.
func streamResults(rd io.Reader, each func(label) error) error {
	dec := json.NewDecoder(rd)
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			continue
		}
		if key, ok := tok.(string); ok && key == "results" && depth == 1 {
			if _, err := dec.Token(); err != nil { // [
				return err
			}
			for dec.More() {
				var l label
				if err := dec.Decode(&l); err != nil {
					return err
				}
				if err := each(l); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// keptLabel is what's remembered about the label kept for a generic name.
type keptLabel struct {
	effective    string
	interactions bool
}

// keepLabel stores a label under its generic name. Many makers sell the
// same drug; the kept label is the newest one that has an interactions
// section, or the newest of all when none has.
func keepLabel(tx *sql.Tx, l label, kept map[string]keptLabel) error {
	if len(l.OpenFDA.Generic) == 0 {
		return nil
	}
	generic := strings.ToLower(strings.TrimSpace(l.OpenFDA.Generic[0]))
	if generic == "" {
		return nil
	}
	interactions := section(l.Interactions)
	has := interactions != ""
	if prev, seen := kept[generic]; seen {
		newer := l.EffectiveTime > prev.effective
		if !(has && !prev.interactions) && !(newer && (has || !prev.interactions)) {
			return nil
		}
	}
	warnings := section(append(l.WarningsCautions, l.Warnings...))
	brands := strings.Join(uniq(l.OpenFDA.Brand), "\n")
	_, err := tx.Exec(`INSERT INTO labels(generic, brands, effective, indications, boxed, contraindications, warnings, interactions)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(generic) DO UPDATE SET brands = excluded.brands, effective = excluded.effective, indications = excluded.indications,
		  boxed = excluded.boxed, contraindications = excluded.contraindications, warnings = excluded.warnings, interactions = excluded.interactions`,
		generic, brands, l.EffectiveTime, section(l.Indications), section(l.Boxed), section(l.Contraindications), warnings, interactions)
	if err == nil {
		kept[generic] = keptLabel{l.EffectiveTime, has}
	}
	return err
}

// flexFloat reads a number written either as a number or as a string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	var v float64
	_, err := fmt.Sscan(s, &v)
	*f = flexFloat(v)
	return err
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		k := strings.ToLower(strings.TrimSpace(s))
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
