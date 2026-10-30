package ingest

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Person is what the checks know about a profile.
type Person struct {
	ID   int64
	Name string
	Born string // YYYY-MM-DD
}

// Check decides whether a result can be filed without a person looking.
// It returns the reasons it can't (empty means file it), and the profile
// the name on it matches better, if any.
func Check(r Result, c Content, owner Person, others []Person, today time.Time) (reasons []string, better *Person) {
	if r.Confidence == "low" {
		reasons = append(reasons, "The reader wasn't sure what this is.")
	}
	if r.PatientName != "" && !NameMatches(owner.Name, r.PatientName) {
		for i := range others {
			if NameMatches(others[i].Name, r.PatientName) {
				better = &others[i]
				break
			}
		}
		if better != nil {
			reasons = append(reasons, fmt.Sprintf("The name on it is “%s”. It looks like %s's, not %s's.", r.PatientName, better.Name, owner.Name))
		} else {
			reasons = append(reasons, fmt.Sprintf("The name on it is “%s”, which doesn't look like %s.", r.PatientName, owner.Name))
		}
	}
	if r.DocDate != "" {
		if d, err := time.Parse("2006-01-02", r.DocDate); err == nil {
			if d.After(today.AddDate(0, 0, 1)) {
				reasons = append(reasons, fmt.Sprintf("The date it found, %s, is in the future.", r.DocDate))
			} else if b, err := time.Parse("2006-01-02", owner.Born); err == nil && d.Before(b) {
				reasons = append(reasons, fmt.Sprintf("The date it found, %s, is before %s was born.", r.DocDate, owner.Name))
			}
		}
	}
	if c.Sampled {
		if c.Rows > 0 {
			reasons = append(reasons, fmt.Sprintf("A table of %d rows: only the first %d were read. Reading whole tables comes with import recipes.", c.Rows, min(c.Rows, maxCSVRows)))
		} else if c.Note != "" {
			reasons = append(reasons, c.Note)
		} else {
			reasons = append(reasons, "Only part of this long file was read.")
		}
	}
	reasons = append(reasons, r.Questions...)
	return reasons, better
}

// NameMatches is a forgiving comparison of a profile name with a name as
// printed ("DOE, JANE M" matches "Jane Doe"). Names are compared by their
// words, ignoring case, punctuation, initials and order; the profile's
// first word must appear, and so must its last when it has one.
func NameMatches(profile, printed string) bool {
	pw, dw := nameWords(profile), nameWords(printed)
	if len(pw) == 0 || len(dw) == 0 {
		return true
	}
	has := func(w string) bool {
		for _, d := range dw {
			if d == w || (len(w) >= 4 && len(d) >= 4 && (strings.HasPrefix(d, w) || strings.HasPrefix(w, d))) {
				return true
			}
		}
		return false
	}
	if !has(pw[0]) {
		return false
	}
	return len(pw) == 1 || has(pw[len(pw)-1])
}

func nameWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		if len([]rune(w)) > 1 {
			out = append(out, w)
		}
	}
	return out
}
