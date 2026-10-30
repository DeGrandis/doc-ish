package reference

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Topic is one MedlinePlus health topic: the NLM-written summary and how
// to find it. The outside sites a topic links to aren't kept.
type Topic struct {
	ID      int64    `json:"id"`
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Also    []string `json:"also,omitempty"`
	Summary string   `json:"summary"`
	Groups  []string `json:"groups,omitempty"`
	Partial bool     `json:"-"` // matched only some words of the question, by name
}

// bundle is the topics as of the last build: a gzipped JSON object
// {"version": "2026-10-29", "topics": [...]}, made by
// `docish reference bundle-medlineplus`.
//
//go:embed data/medlineplus.json.gz
var bundle []byte

type bundleFile struct {
	Version string  `json:"version"`
	Topics  []Topic `json:"topics"`
}

func decodeBundle(b []byte) (bundleFile, error) {
	var f bundleFile
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return f, err
	}
	err = json.NewDecoder(zr).Decode(&f)
	return f, err
}

// loadBundled loads the built-in topics when the database has none, or
// older ones.
func (r *Ref) loadBundled() error {
	if len(bundle) == 0 {
		return nil
	}
	f, err := decodeBundle(bundle)
	if err != nil {
		return fmt.Errorf("bundled MedlinePlus topics: %w", err)
	}
	if v := r.version(MedlinePlus); v != "" && v >= f.Version {
		return nil
	}
	return r.ImportTopics(f.Version, f.Topics)
}

// ImportTopics replaces the topics.
func (r *Ref) ImportTopics(version string, topics []Topic) error {
	return r.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM topics; INSERT INTO topics_fts(topics_fts) VALUES('delete-all')`); err != nil {
			return err
		}
		for _, t := range topics {
			also := strings.Join(t.Also, "\n")
			if _, err := tx.Exec(`INSERT INTO topics(id, title, url, also, summary, groups) VALUES(?, ?, ?, ?, ?, ?)`,
				t.ID, t.Title, t.URL, also, t.Summary, strings.Join(t.Groups, "\n")); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO topics_fts(rowid, title, also, summary) VALUES(?, ?, ?, ?)`, t.ID, t.Title, also, t.Summary); err != nil {
				return err
			}
		}
		return setSource(tx, MedlinePlus, version, len(topics))
	})
}

// ParseTopics reads the MedlinePlus health topics XML, keeping English
// topics.
func ParseTopics(rd io.Reader) ([]Topic, error) {
	dec := xml.NewDecoder(rd)
	var out []Topic
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "health-topic" {
			continue
		}
		var x struct {
			ID       int64    `xml:"id,attr"`
			Title    string   `xml:"title,attr"`
			URL      string   `xml:"url,attr"`
			Language string   `xml:"language,attr"`
			Also     []string `xml:"also-called"`
			See      []string `xml:"see-reference"`
			Summary  string   `xml:"full-summary"`
			Groups   []string `xml:"group"`
		}
		if err := dec.DecodeElement(&x, &se); err != nil {
			return nil, err
		}
		if x.Language != "English" {
			continue
		}
		out = append(out, Topic{
			ID: x.ID, Title: x.Title, URL: x.URL, Also: append(x.Also, x.See...),
			Summary: htmlToText(x.Summary), Groups: x.Groups,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("no English health topics in the file")
	}
	return out, nil
}

var (
	tagRE    = regexp.MustCompile(`<[^>]*>`)
	spacesRE = regexp.MustCompile(`[ \t]+`)
	blankRE  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText turns a topic summary's simple HTML into plain text with
// list items as "- " lines.
func htmlToText(s string) string {
	r := strings.NewReplacer("<li>", "\n- ", "</li>", "", "<p>", "\n\n", "</p>", "", "<br>", "\n", "<br/>", "\n", "<br />", "\n")
	s = r.Replace(s)
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spacesRE.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(blankRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

var mplusZipRE = regexp.MustCompile(`xml/mplus_topics_compressed_(\d{4}-\d{2}-\d{2})\.zip`)

// FetchTopics downloads the latest topics from medlineplus.gov.
func FetchTopics(ctx context.Context, c *http.Client) (string, []Topic, error) {
	page, err := get(ctx, c, "https://medlineplus.gov/xml.html", 2<<20)
	if err != nil {
		return "", nil, err
	}
	m := mplusZipRE.FindSubmatch(page)
	if m == nil {
		return "", nil, errors.New("couldn't find the topics file on medlineplus.gov/xml.html")
	}
	version := string(m[1])
	zb, err := get(ctx, c, "https://medlineplus.gov/"+string(m[0]), 64<<20)
	if err != nil {
		return "", nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		return "", nil, err
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".xml") {
			rc, err := f.Open()
			if err != nil {
				return "", nil, err
			}
			defer rc.Close()
			topics, err := ParseTopics(rc)
			return version, topics, err
		}
	}
	return "", nil, errors.New("the topics zip had no XML in it")
}

// BundleTopics writes the bundle file for the binary.
func BundleTopics(w io.Writer, version string, topics []Topic) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(zw).Encode(bundleFile{Version: version, Topics: topics}); err != nil {
		return err
	}
	return zw.Close()
}

func get(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
