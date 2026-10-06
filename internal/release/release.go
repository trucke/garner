// Package release reads what a TV or anime release name claims about itself.
//
// It never consults a catalogue. Mapping claims to TVDB episodes, reconciling
// them with indexer metadata and deciding whether to download happen elsewhere.
package release

import (
	"fmt"
	"strings"
)

// MaxNameLength bounds untrusted input. Longer names yield no claims.
const MaxNameLength = 512

// maxEpisodes bounds how many episodes a single range may expand to.
const maxEpisodes = 500

// Span is a byte range in Claims.Name.
type Span struct{ Start, End int }

// Claim is one piece of evidence and where the name states it.
type Claim[T comparable] struct {
	Value T
	Span  Span
}

// State tells how much a set of claims agrees.
type State int

const (
	Missing State = iota
	Unambiguous
	Conflicting
)

// Value returns the claimed value when all claims agree.
func Value[T comparable](claims []Claim[T]) (T, State) {
	var zero T
	if len(claims) == 0 {
		return zero, Missing
	}
	for _, c := range claims[1:] {
		if c.Value != claims[0].Value {
			return zero, Conflicting
		}
	}
	return claims[0].Value, Unambiguous
}

// Claims is everything a release name states. Each field lists claims in the
// order they appear; empty means the name says nothing about it.
type Claims struct {
	Name string

	// Series holds title candidates: the full title first, then its parts when
	// the name separates alternative titles with " - ". When a number may end
	// the title or be the episode, as in "Show.101" or "[Group] Title 01", the
	// candidate including the number comes last; its span overlaps the
	// numbering claim, so only one of the two can be right.
	Series       []Claim[string]
	EpisodeTitle []Claim[string]
	Numbering    []Numbering
	// Extras marks content that is not a regular episode, such as specials or
	// creditless openings. Such releases never satisfy episodes.
	Extras []Claim[string]

	Resolution []Claim[Resolution]
	Source     []Claim[Source]
	Year       []Claim[int]
	Group      []Claim[string]
	Version    []Claim[int]
	Flags      []Claim[Flag]
	// Services names the streaming service a web release came from. TVDB
	// may number a show's episodes differently for that service.
	Services []Claim[Service]

	// Audio and Subtitles name languages. A named language says nothing about
	// which other languages are present.
	Audio     []Claim[Language]
	Subtitles []Claim[Language]
	// Markers state multiple or dubbed tracks without naming languages.
	Markers []Claim[Marker]
}

// Numbering is one episode or season claim. A name may contain several,
// for example an absolute number next to a season and episode; those must
// agree. Claims with the same span are alternative readings of one number,
// such as 101 read as S01E01 or as episode 101; at most one is right.
type Numbering struct {
	Season   int
	Seasoned bool
	// Episodes is empty when the claim covers a whole season, which says
	// nothing about whether every episode is included.
	Episodes []Episode
	Span     Span
}

// Episode is an episode number. Recaps can carry a fraction, as in 18.5.
type Episode struct{ Number, Fraction int }

func (e Episode) String() string {
	if e.Fraction > 0 {
		return fmt.Sprintf("%02d.%d", e.Number, e.Fraction)
	}
	return fmt.Sprintf("%02d", e.Number)
}

// String renders the claim compactly, for example S01E01, S01E158-E170,
// S06 or E29.
func (n Numbering) String() string {
	var b strings.Builder
	if n.Seasoned {
		fmt.Fprintf(&b, "S%02d", n.Season)
	}
	for i := 0; i < len(n.Episodes); {
		j := i
		for j+1 < len(n.Episodes) && n.Episodes[j].Fraction == 0 && n.Episodes[j+1].Fraction == 0 &&
			n.Episodes[j+1].Number == n.Episodes[j].Number+1 {
			j++
		}
		b.WriteString("E" + n.Episodes[i].String())
		if j > i {
			b.WriteString("-E" + n.Episodes[j].String())
		}
		i = j + 1
	}
	return b.String()
}

// Resolution is the vertical resolution, for example 1080.
type Resolution int

func (r Resolution) String() string { return fmt.Sprintf("%dp", int(r)) }

type Source string

// Service is a streaming service, such as netflix.
type Service string

const (
	BluRay Source = "bluray"
	WebDL  Source = "webdl"
	WebRip Source = "webrip"
	// Web is a web release that does not say whether it is WEB-DL or WEBRip.
	Web   Source = "web"
	HDTV  Source = "hdtv"
	TV    Source = "tv" // standard-definition broadcast or satellite capture
	DVD   Source = "dvd"
	HDRip Source = "hdrip"
)

// Language is an ISO 639-1 code, or Original.
type Language string

// Original is the show's original language. German releases mark it with DL.
const Original Language = "original"

type Marker string

const (
	DualAudio  Marker = "dual-audio"
	MultiAudio Marker = "multi-audio"
	Dubbed     Marker = "dubbed"
	MultiSubs  Marker = "multi-subs"
	Subs       Marker = "subs"
)

type Flag string

const (
	Remux       Flag = "remux"
	Repack      Flag = "repack"
	Proper      Flag = "proper"
	Internal    Flag = "internal"
	Complete    Flag = "complete"
	Batch       Flag = "batch"
	Uncensored  Flag = "uncensored"
	HDR         Flag = "hdr"
	DolbyVision Flag = "dv"
)
