package ingest

import (
	"sort"
	"strings"
)

// A records release is one file with many records in it: visit notes,
// lab reports, letters, handouts. Each part of a long file says which
// record each of its pages belongs to; here neighbouring pages of one
// record are joined into a section, so a visit on its own date shows as a
// visit, not as page 8 of something.

// sectionsOf joins the parts' page labels into sections, in page order.
// A new section starts where the model says a record starts, or where the
// kind changes, or (outside lab tables, which hold many dates) the date.
func sectionsOf(parts []partResult) []Section {
	labels := map[int]PageLabel{}
	for _, p := range parts {
		if p.err != nil {
			continue
		}
		for _, l := range p.res.Pages {
			if _, ok := labels[l.Page]; !ok && l.Page >= p.first && l.Page <= p.last {
				labels[l.Page] = l
			}
		}
	}
	var pages []int
	for n := range labels {
		pages = append(pages, n)
	}
	sort.Ints(pages)

	var out []Section
	var titles []map[string]int
	var abouts [][]string
	for _, n := range pages {
		l := labels[n]
		k := len(out) - 1
		// The next page of one dated record with a heading of its own
		// ("Plan:") isn't a new record, whatever the model says.
		same := k >= 0 && l.Kind == out[k].Kind && l.Date != "" && l.Date == out[k].Date && n == out[k].Last+1
		if k < 0 || !same && (l.NewRecord || l.Kind != out[k].Kind || l.Date != "" && out[k].Date != "" && l.Date != out[k].Date && l.Kind != "lab") {
			out = append(out, Section{First: n, Kind: l.Kind})
			titles, abouts = append(titles, map[string]int{}), append(abouts, nil)
			k++
		}
		s := &out[k]
		s.Last = n
		if s.Date == "" {
			s.Date = l.Date
		}
		if s.Provider == "" {
			s.Provider = l.Provider
		}
		if l.Title != "" {
			titles[k][l.Title]++
			if s.Title == "" {
				s.Title = l.Title // the first page's, unless most pages say otherwise
			}
		}
		if l.About != "" {
			abouts[k] = append(abouts[k], l.About)
		}
	}
	for k := range out {
		if t := most(titles[k], out[k].Title); titles[k][t] > titles[k][out[k].Title] {
			out[k].Title = t
		}
		out[k].Summary = clip(strings.Join(abouts[k], " "), 800)
	}
	return out
}

// sectionFor is the section (from 1) a part's value or follow-up belongs
// to: of the sections on the part's pages, the one of its date, else for
// a value the lab one, else the first. 0 when there are none.
func sectionFor(secs []Section, p part, date string, value bool) int {
	first, lab := 0, 0
	for i, s := range secs {
		if s.Last < p.first || s.First > p.last {
			continue
		}
		if first == 0 {
			first = i + 1
		}
		if date != "" && s.Date == date {
			return i + 1
		}
		if s.Kind == "lab" && lab == 0 {
			lab = i + 1
		}
	}
	if value && lab != 0 {
		return lab
	}
	return first
}

// finishSections dates a lab section by its newest result (a cumulative
// table holds many), and drops sections from a file that holds only one
// record: the file is that record.
func finishSections(r *Result) {
	if len(r.Sections) <= 1 {
		r.Sections = nil
		for i := range r.Observations {
			r.Observations[i].Section = 0
		}
		for i := range r.FollowUps {
			r.FollowUps[i].Section = 0
		}
		return
	}
	for _, v := range r.Observations {
		if v.Section > 0 {
			s := &r.Sections[v.Section-1]
			if s.Kind == "lab" && v.Date > s.Date {
				s.Date = v.Date
			}
		}
	}
}
