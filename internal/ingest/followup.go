package ingest

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Small models are poor at date arithmetic, so a follow-up's timing is
// taken as written ("in about six months") and its due date worked out
// here, from the date of the note that said it.

var (
	inPeriod = regexp.MustCompile(`(?i)\b(\d+|a|an|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|eighteen|twenty-four)(?:\s*(?:-|to|or)\s*(?:\d+|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve))?\s+(day|week|month|year)s?\b`)
	yearly   = regexp.MustCompile(`(?i)\b(annual(ly)?|yearly|next year|every year)\b`)
	numbers  = map[string]int{"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
		"nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "eighteen": 18, "twenty-four": 24}
)

// DueDate works out when a follow-up is due: "in about six months" from a
// note of 2025-09-12 is 2026-03-12. A range ("6-12 months") counts from
// its start. Empty when the timing isn't a period ("next fall") or there's
// no date to count from.
func DueDate(when, from string) string {
	base, err := time.Parse("2006-01-02", from)
	if err != nil {
		return ""
	}
	n, unit := 0, ""
	if m := inPeriod.FindStringSubmatch(when); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil {
			n = v
		} else {
			n = numbers[strings.ToLower(m[1])]
		}
		unit = strings.ToLower(m[2])
	} else if yearly.MatchString(when) {
		n, unit = 1, "year"
	}
	if n <= 0 || n > 120 {
		return ""
	}
	switch unit {
	case "day":
		base = base.AddDate(0, 0, n)
	case "week":
		base = base.AddDate(0, 0, 7*n)
	case "month":
		base = base.AddDate(0, n, 0)
	case "year":
		base = base.AddDate(n, 0, 0)
	default:
		return ""
	}
	return base.Format("2006-01-02")
}
