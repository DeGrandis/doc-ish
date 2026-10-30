package reference

import (
	"archive/zip"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var loincVersionRE = regexp.MustCompile(`(?i)loinc[_ ]?(\d+\.\d+)`)

// ImportLOINC loads lab tests from the LOINC release zip the user
// downloaded from loinc.org: active laboratory terms only, leaving out
// any whose content belongs to a third party (those carry their own
// copyright notice).
func (r *Ref) ImportLOINC(zipPath, filename string) (int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, errors.New("that isn't a zip file; drop the LOINC release zip from loinc.org")
	}
	defer zr.Close()
	var table *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "LoincTable/Loinc.csv") || strings.EqualFold(f.Name, "Loinc.csv") {
			table = f
		}
	}
	if table == nil {
		return 0, errors.New("no LoincTable/Loinc.csv in that zip; it should be the full LOINC release from loinc.org")
	}
	rc, err := table.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	version := "imported"
	if m := loincVersionRE.FindStringSubmatch(filename); m != nil {
		version = m[1]
	}
	n := 0
	err = r.tx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM loinc`, `INSERT INTO loinc_fts(loinc_fts) VALUES('delete-all')`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		cr := csv.NewReader(rc)
		cr.FieldsPerRecord = -1
		head, err := cr.Read()
		if err != nil {
			return err
		}
		col := map[string]int{}
		for i, h := range head {
			col[strings.ToUpper(strings.Trim(strings.TrimPrefix(h, "\xef\xbb\xbf"), `" `))] = i
		}
		for _, need := range []string{"LOINC_NUM", "COMPONENT", "LONG_COMMON_NAME", "CLASSTYPE", "STATUS"} {
			if _, ok := col[need]; !ok {
				return fmt.Errorf("Loinc.csv has no %s column", need)
			}
		}
		get := func(rec []string, name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		for {
			rec, err := cr.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if get(rec, "CLASSTYPE") != "1" || get(rec, "STATUS") != "ACTIVE" || get(rec, "EXTERNAL_COPYRIGHT_NOTICE") != "" {
				continue
			}
			res, err := tx.Exec(`INSERT INTO loinc(code, component, name, short, class, units, related) VALUES(?, ?, ?, ?, ?, ?, ?)`,
				get(rec, "LOINC_NUM"), get(rec, "COMPONENT"), get(rec, "LONG_COMMON_NAME"), get(rec, "SHORTNAME"),
				get(rec, "CLASS"), get(rec, "EXAMPLE_UCUM_UNITS"), get(rec, "RELATEDNAMES2"))
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			if _, err := tx.Exec(`INSERT INTO loinc_fts(rowid, name, short, component, related) VALUES(?, ?, ?, ?, ?)`,
				id, get(rec, "LONG_COMMON_NAME"), get(rec, "SHORTNAME"), get(rec, "COMPONENT"), get(rec, "RELATEDNAMES2")); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return errors.New("no active lab tests found in Loinc.csv")
		}
		return setSource(tx, LOINC, version, n)
	})
	return n, err
}
