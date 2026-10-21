package selection

import (
	"testing"
	"time"

	"github.com/trucke/garner/internal/release"
)

// FuzzEvaluate judges arbitrary names, with an arbitrary language the
// indexer may report, under the TV and anime profiles and ranks the two
// verdicts. The invariants on the evaluation are checked inside Evaluate,
// so a broken one fails the fuzzer with the panic itself. The published
// time is always zero, keeping runs deterministic.
func FuzzEvaluate(f *testing.F) {
	for _, tc := range evaluateTests {
		f.Add(tc.name, "")
	}
	f.Add("Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP", "German")
	f.Fuzz(func(t *testing.T, name, language string) {
		show, want := fixture("Harbor Precinct", "en")
		claims := release.Parse(name)
		r := Release{
			Claims:           claims,
			Match:            show.Match(claims, time.Time{}),
			IndexerLanguages: IndexerLanguages([]string{language}),
		}
		evals := []Evaluation{
			Evaluate(Context{Profile: TV, Show: show, Want: want, Original: "en"}, r),
			Evaluate(Context{Profile: Anime, Show: show, Want: want, Original: "ja"}, r),
		}
		Rank(evals)
	})
}
