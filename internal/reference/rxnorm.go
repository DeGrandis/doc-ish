package reference

import (
	"archive/zip"
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Progress reports a long download or import: what it's doing and, when
// known, how far along it is.
type Progress func(stage string, done, total int64)

var rxZipRE = regexp.MustCompile(`https://download\.nlm\.nih\.gov/rxnorm/RxNorm_full_prescribe_(\d{2})(\d{2})(\d{4})\.zip`)

// keptTTY are the RxNorm term types worth keeping: ingredients, brands,
// and products with their strength and form.
var keptTTY = map[string]bool{"IN": true, "PIN": true, "MIN": true, "BN": true, "SCD": true, "SBD": true, "GPCK": true, "BPCK": true}

// FetchRxNorm downloads RxNorm Current Prescribable Content (no license
// needed) and loads its names and brand-ingredient links.
func (r *Ref) FetchRxNorm(ctx context.Context, c *http.Client, tmpDir string, progress Progress) error {
	progress("Finding the latest release", 0, 0)
	page, err := get(ctx, c, "https://www.nlm.nih.gov/research/umls/rxnorm/docs/rxnormfiles.html", 4<<20)
	if err != nil {
		return err
	}
	m := rxZipRE.FindSubmatch(page)
	if m == nil {
		return errors.New("couldn't find the prescribable release on the RxNorm files page")
	}
	version := fmt.Sprintf("%s-%s-%s", m[3], m[1], m[2])
	path, err := download(ctx, c, string(m[0]), tmpDir, "Downloading RxNorm", progress)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	progress("Loading drug names", 0, 0)
	return r.importRxNorm(path, version)
}

func (r *Ref) importRxNorm(zipPath, version string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	open := func(suffix string) (io.ReadCloser, error) {
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, suffix) {
				return f.Open()
			}
		}
		return nil, fmt.Errorf("the RxNorm zip has no %s", suffix)
	}
	conso, err := open("RXNCONSO.RRF")
	if err != nil {
		return err
	}
	defer conso.Close()
	rel, err := open("RXNREL.RRF")
	if err != nil {
		return err
	}
	defer rel.Close()

	return r.tx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM drugs`, `INSERT INTO drugs_fts(drugs_fts) VALUES('delete-all')`, `DELETE FROM brands`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		tty := map[string]string{} // rxcui -> its kept term type
		n := 0
		sc := bufio.NewScanner(conso)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			// RXCUI|LAT|TS|LUI|STT|SUI|ISPREF|RXAUI|SAUI|SCUI|SDUI|SAB|TTY|CODE|STR|SRL|SUPPRESS|CVF|
			f := strings.Split(sc.Text(), "|")
			if len(f) < 17 || f[1] != "ENG" || f[11] != "RXNORM" || !keptTTY[f[12]] || (f[16] != "N" && f[16] != "") {
				continue
			}
			res, err := tx.Exec(`INSERT INTO drugs(rxcui, tty, name) VALUES(?, ?, ?)`, f[0], f[12], f[14])
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			if _, err := tx.Exec(`INSERT INTO drugs_fts(rowid, name) VALUES(?, ?)`, id, f[14]); err != nil {
				return err
			}
			tty[f[0]] = f[12]
			n++
		}
		if err := sc.Err(); err != nil {
			return err
		}
		sc = bufio.NewScanner(rel)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			// RXCUI1|RXAUI1|STYPE1|REL|RXCUI2|RXAUI2|STYPE2|RELA|…
			f := strings.Split(sc.Text(), "|")
			if len(f) < 8 || (f[7] != "tradename_of" && f[7] != "has_tradename") {
				continue
			}
			a, b := f[0], f[4]
			// Orient by term type rather than trusting the direction.
			if tty[a] == "BN" && (tty[b] == "IN" || tty[b] == "PIN" || tty[b] == "MIN") {
				a, b = b, a
			}
			if (tty[a] == "IN" || tty[a] == "PIN" || tty[a] == "MIN") && tty[b] == "BN" {
				if _, err := tx.Exec(`INSERT INTO brands(brand, ingredient) VALUES(?, ?)`, b, a); err != nil {
					return err
				}
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		if n == 0 {
			return errors.New("no drug names found in the RxNorm files")
		}
		return setSource(tx, RxNorm, version, n)
	})
}

// download saves url to a temporary file, reporting progress.
func download(ctx context.Context, c *http.Client, url, tmpDir, stage string, progress Progress) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	f, err := os.CreateTemp(tmpDir, "reference-*.zip")
	if err != nil {
		return "", err
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(f.Name())
				return "", werr
			}
			done += int64(n)
			progress(stage, done, total)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
