package match

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// germanLetters are written this way in release names.
var germanLetters = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "&", " and ")

// romanNumerals become digits. I, V and X stay words: "I" and "X" appear in
// titles as words ("LAMP x HARBOR").
var romanNumerals = map[string]string{"ii": "2", "iii": "3", "iv": "4", "vi": "6", "vii": "7", "viii": "8", "ix": "9"}

// Words normalises a title for comparison: lower case, German letters as
// release names write them, no accents, & as "and", Roman numerals as digits
// and punctuation dropped. Apostrophes join ("Journey's" is "journeys"); other
// punctuation separates words.
func Words(s string) []string {
	s = germanLetters.Replace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r), r == '\'', r == '’', r == 'ʼ', r == '`':
			// accents and apostrophes
		case unicode.IsDigit(r), unicode.IsLetter(r) && !unicode.Is(unicode.Lm, r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	for i, w := range words {
		if d, ok := romanNumerals[w]; ok {
			words[i] = d
		}
	}
	return words
}

// text is a normalised title.
type text struct {
	words   []string
	compact string // words without spaces: "Hakai Shin" equals "Hakaishin"
}

func newText(s string) text {
	w := Words(s)
	return text{words: w, compact: strings.Join(w, "")}
}

func (t text) empty() bool { return len(t.words) == 0 }

// equal reports whether two titles are the same after normalisation.
func (t text) equal(o text) bool { return !t.empty() && t.compact == o.compact }

// close reports whether one title is the other plus one word, such as
// "After the Winters End" and "After Winters End". More extra words often
// name a spin-off ("Harbor Precinct Mayors Week at Work"), so they do not
// match.
// The shorter title needs at least minWords words.
func (t text) close(o text, minWords int) bool {
	short, long := t.words, o.words
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < minWords || len(long) != len(short)+1 {
		return false
	}
	have := make(map[string]int, len(long))
	for _, w := range long {
		have[w]++
	}
	for _, w := range short {
		if have[w] == 0 {
			return false
		}
		have[w]--
	}
	return true
}
