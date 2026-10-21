package match

import (
	"testing"
	"time"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// FuzzMatch matches arbitrary names against the made-up shows of the test
// cases. The invariants on the result are checked inside Match, so a
// broken one fails the fuzzer with the panic itself. The published time is
// always zero, keeping runs deterministic.
func FuzzMatch(f *testing.F) {
	for _, tc := range matchTests {
		f.Add(tc.name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		claims := release.Parse(name)
		for _, build := range []func() store.SeriesData{lanternHarbor, harborPrecinct, precinct100} {
			for _, opts := range []Options{{}, anime} {
				show := NewShow(build(), opts)
				m := show.Match(claims, time.Time{})
				_ = summarize(show, m)
			}
		}
	})
}
