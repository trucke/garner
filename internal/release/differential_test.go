package release

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	jhin "github.com/dreulavelle/jhin/parser"
)

// TestJhinDifferential compares Parse with Jhin over a file of release names,
// one per line, named by GARNER_RELEASE_NAMES. Disagreements are leads for
// review, not failures: either parser may be wrong. Run with -v to see them.
func TestJhinDifferential(t *testing.T) {
	path := os.Getenv("GARNER_RELEASE_NAMES")
	if path == "" {
		t.Skip("GARNER_RELEASE_NAMES not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	differences := map[string][]string{}
	total := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" {
			continue
		}
		total++
		ours, theirs := Parse(name), jhin.Parse(name)
		compare := func(field, a, b string) {
			if a != b {
				differences[field] = append(differences[field], fmt.Sprintf("ours %-28q jhin %-28q %s", a, b, name))
			}
		}
		compare("numbering", ourNumbering(ours), jhinNumbering(theirs))
		compare("series", normalise(first(ours.Series)), normalise(theirs.Title))
		compare("resolution", ourResolution(ours), strings.ToLower(strings.Replace(theirs.Resolution, "4k", "2160p", 1)))
		compare("year", ourYear(ours), theirs.Year)
		compare("group", first(ours.Group), theirs.Group)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 0, len(differences))
	for k := range differences {
		fields = append(fields, k)
	}
	slices.Sort(fields)
	t.Logf("%d names", total)
	for _, k := range fields {
		t.Logf("%s: %d differences", k, len(differences[k]))
		for _, d := range differences[k] {
			t.Logf("  %s", d)
		}
	}
}

// ourNumbering renders the first claim that Jhin could also express.
func ourNumbering(c Claims) string {
	for _, n := range c.Numbering {
		s := numberingKey(n.Seasoned, n.Season, nil)
		eps := make([]int, 0, len(n.Episodes))
		for _, e := range n.Episodes {
			if e.Fraction != 0 {
				return "fraction"
			}
			eps = append(eps, e.Number)
		}
		return s + numberingKey(false, 0, eps)
	}
	return ""
}

func jhinNumbering(r *jhin.Result) string {
	if len(r.Seasons) == 0 && len(r.Episodes) == 0 {
		return ""
	}
	if len(r.Seasons) > 1 {
		return fmt.Sprint("seasons ", r.Seasons)
	}
	return numberingKey(len(r.Seasons) == 1, firstInt(r.Seasons), nil) + numberingKey(false, 0, r.Episodes)
}

func numberingKey(seasoned bool, season int, eps []int) string {
	var b strings.Builder
	if seasoned {
		fmt.Fprintf(&b, "S%02d", season)
	}
	for _, e := range eps {
		fmt.Fprintf(&b, "E%02d", e)
	}
	return b.String()
}

func ourResolution(c Claims) string {
	if r, s := Value(c.Resolution); s == Unambiguous {
		return r.String()
	}
	return ""
}

func ourYear(c Claims) string {
	if y, s := Value(c.Year); s == Unambiguous {
		return strconv.Itoa(y)
	}
	return ""
}

func first(claims []Claim[string]) string {
	if len(claims) == 0 {
		return ""
	}
	return claims[0].Value
}

func firstInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	return v[0]
}

// normalise compares titles by letters and digits only.
func normalise(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
