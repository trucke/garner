// Package search finds releases for an episode: it queries every indexer
// through Prowlarr, matches each result to the catalogue, judges it against
// the show's profile and stores the ranked outcome.
package search

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/prowlarr"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// Service runs searches.
type Service struct {
	store    *store.Store
	prowlarr *prowlarr.Client
	// rules returns a show's reviewed streaming-service order rules.
	rules func(seriesID int64) map[release.Service]string
	log   *slog.Logger
	now   func() time.Time
	// OnDone is called after a search was stored.
	OnDone func(store.Search)
	// OnProgress is called when a search starts asking the indexers and
	// after each of them answered.
	OnProgress func(Progress)
}

// Progress is a running search: every indexer, and what it answered so far.
// A season search sets SeriesID and Season instead of EpisodeID.
type Progress struct {
	EpisodeID int64             `json:"episode_id"`
	SeriesID  int64             `json:"series_id,omitempty"`
	Season    int               `json:"season,omitempty"`
	Indexers  []IndexerProgress `json:"indexers"`
}

// IndexerProgress is one indexer of a running search.
type IndexerProgress struct {
	Name    string `json:"name"`
	Done    bool   `json:"done"`
	Results int    `json:"results"`
	Error   string `json:"error,omitempty"`
}

// New returns a search service. rules may be nil.
func New(st *store.Store, p *prowlarr.Client, rules func(int64) map[release.Service]string, log *slog.Logger) *Service {
	if rules == nil {
		rules = func(int64) map[release.Service]string { return nil }
	}
	return &Service{store: st, prowlarr: p, rules: rules, log: log, now: time.Now}
}

// Episode searches for an episode and stores the outcome, replacing the
// previous search.
func (s *Service) Episode(ctx context.Context, episodeID int64) (store.Search, error) {
	seriesID, err := s.store.SeriesOfEpisode(ctx, episodeID)
	if err != nil {
		return store.Search{}, err
	}
	d, err := s.store.LoadSeries(ctx, seriesID)
	if err != nil {
		return store.Search{}, err
	}
	i := slices.IndexFunc(d.Episodes, func(e store.Episode) bool { return e.ID == episodeID })
	if i < 0 {
		return store.Search{}, fmt.Errorf("episode %d: %w", episodeID, store.ErrNotFound)
	}
	ep := d.Episodes[i]
	pos, ok := ep.Numbers["official"]
	if !ok {
		return store.Search{}, fmt.Errorf("episode %d has no aired-order position to search for", episodeID)
	}

	files, err := s.store.SeriesFileTiers(ctx, seriesID)
	if err != nil {
		return store.Search{}, err
	}
	start := s.now()
	profile := selection.ProfileNamed(d.Series.Profile)
	show := match.NewShow(d, match.Options{Anime: profile.Name == selection.Anime.Name, ServiceOrders: s.rules(seriesID)})
	original, _ := release.LanguageNamed(d.Series.OriginalLanguage)
	judge := selection.Context{Profile: profile, Show: show, Want: episodeID, Original: original,
		Missing: missing(d, seasonEpisodes(d, "official", pos.Season), files, start)}

	sr := store.Search{EpisodeID: episodeID, Searched: start, Indexers: []store.SearchIndexer{}}
	var hits []hit
	indexers, err := s.prowlarr.Indexers(ctx)
	if err != nil {
		sr.Indexers = append(sr.Indexers, store.SearchIndexer{Name: "Prowlarr", Error: err.Error()})
	}
	progress := Progress{EpisodeID: episodeID, Indexers: make([]IndexerProgress, len(indexers))}
	for i, ix := range indexers {
		progress.Indexers[i] = IndexerProgress{Name: ix.Name}
	}
	s.report(progress)
	for i, ix := range indexers {
		results, err := s.prowlarr.Search(ctx, ix.ID, prowlarr.Query{TVDBID: seriesID, Season: pos.Season, Episode: pos.Number})
		if ctx.Err() != nil {
			return store.Search{}, ctx.Err()
		}
		res := store.SearchIndexer{Name: ix.Name, Results: len(results)}
		if err != nil {
			res.Error = err.Error()
			s.log.Warn("indexer search failed", "indexer", ix.Name, "episode", episodeID, "err", err)
		}
		sr.Indexers = append(sr.Indexers, res)
		progress.Indexers[i] = IndexerProgress{Name: ix.Name, Done: true, Results: res.Results, Error: res.Error}
		s.report(progress)
		for _, r := range results {
			claims := release.Parse(r.Title)
			m := show.Match(claims, r.Published)
			eval := selection.Evaluate(judge, selection.Release{
				Claims: claims, Match: m, IndexerLanguages: selection.IndexerLanguages(r.Languages),
				OtherShow: r.TVDBID != 0 && r.TVDBID != seriesID, Published: r.Published,
				Size: r.Size, Runtime: runtimeOf(d, m.Episodes),
			})
			hits = append(hits, hit{indexer: ix.Name, result: r, eval: eval, episodes: m.Episodes})
		}
	}

	evals := make([]selection.Evaluation, len(hits))
	for i, h := range hits {
		evals[i] = h.eval
	}
	sr.Candidates = []store.Candidate{}
	for _, i := range selection.Rank(evals) {
		sr.Candidates = append(sr.Candidates, hits[i].candidate())
	}
	sr.Outcome, sr.Summary = outcome(selection.Summary(profile, evals), evals, sr.Indexers)
	if sr.Outcome != "found" {
		retry := start.Add(store.RetryDelay(sr.Outcome, ep.Aired, start))
		sr.NextRetry = &retry
	}
	if err := s.store.SaveSearch(ctx, sr); err != nil {
		return store.Search{}, err
	}
	saved, err := s.store.LoadSearch(ctx, episodeID)
	if err != nil {
		return store.Search{}, err
	}
	s.log.Info("searched", "series", seriesID, "episode", show.Label(episodeID), "outcome", saved.Outcome,
		"candidates", len(saved.Candidates), "duration", s.now().Sub(start).Round(time.Millisecond))
	if s.OnDone != nil {
		s.OnDone(saved)
	}
	return saved, nil
}

// Season searches a whole season for packs: releases that hold several of
// its episodes. It asks each indexer once for the season; single episodes
// are left to the episode search, which asks for them precisely, while a
// season's answer may stop at Prowlarr's page limit on popular shows. The
// season is one of the show's default order, as the show lists it; the
// indexers are asked for the aired-order seasons its episodes belong to,
// usually just the one. The outcome replaces the season's previous search.
func (s *Service) Season(ctx context.Context, seriesID int64, season int) (store.Search, error) {
	d, err := s.store.LoadSeries(ctx, seriesID)
	if err != nil {
		return store.Search{}, err
	}
	episodes := seasonEpisodes(d, d.Series.DefaultOrder, season)
	var aired []int // the seasons indexers know them by
	for _, e := range d.Episodes {
		if p, ok := e.Numbers["official"]; ok && p.Season > 0 && slices.Contains(episodes, e.ID) && !slices.Contains(aired, p.Season) {
			aired = append(aired, p.Season)
		}
	}
	slices.Sort(aired)
	if season < 1 || len(aired) == 0 {
		return store.Search{}, fmt.Errorf("season %d: %w", season, store.ErrNotFound)
	}
	files, err := s.store.SeriesFileTiers(ctx, seriesID)
	if err != nil {
		return store.Search{}, err
	}
	start := s.now()
	wanted := missing(d, episodes, files, start)
	profile := selection.ProfileNamed(d.Series.Profile)
	show := match.NewShow(d, match.Options{Anime: profile.Name == selection.Anime.Name, ServiceOrders: s.rules(seriesID)})
	original, _ := release.LanguageNamed(d.Series.OriginalLanguage)

	sr := store.Search{Season: season, Searched: start, Indexers: []store.SearchIndexer{}}
	var hits []hit
	indexers, err := s.prowlarr.Indexers(ctx)
	if err != nil {
		sr.Indexers = append(sr.Indexers, store.SearchIndexer{Name: "Prowlarr", Error: err.Error()})
	}
	progress := Progress{SeriesID: seriesID, Season: season, Indexers: make([]IndexerProgress, len(indexers))}
	for i, ix := range indexers {
		progress.Indexers[i] = IndexerProgress{Name: ix.Name}
	}
	s.report(progress)
	for i, ix := range indexers {
		var results []prowlarr.Result
		var err error
		seen := map[string]bool{} // by link: a pack can span two seasons
		for _, n := range aired {
			var found []prowlarr.Result
			found, err = s.prowlarr.Search(ctx, ix.ID, prowlarr.Query{TVDBID: seriesID, Season: n})
			for _, r := range found {
				if !seen[r.Link] {
					seen[r.Link] = true
					results = append(results, r)
				}
			}
			if err != nil {
				break
			}
		}
		if ctx.Err() != nil {
			return store.Search{}, ctx.Err()
		}
		res := store.SearchIndexer{Name: ix.Name, Results: len(results)}
		if err != nil {
			res.Error = err.Error()
			s.log.Warn("indexer search failed", "indexer", ix.Name, "series", seriesID, "season", season, "err", err)
		}
		sr.Indexers = append(sr.Indexers, res)
		progress.Indexers[i] = IndexerProgress{Name: ix.Name, Done: true, Results: res.Results, Error: res.Error}
		s.report(progress)
		for _, r := range results {
			claims := release.Parse(r.Title)
			m := show.Match(claims, r.Published)
			held := slices.DeleteFunc(slices.Clone(episodes), func(id int64) bool { return !slices.Contains(m.Episodes, id) })
			if len(m.Episodes) < 2 || len(held) == 0 {
				continue // a single episode, or another season's
			}
			// Judged for its first missing episode, which a download of it
			// then belongs to.
			owner := held[0]
			if k := slices.IndexFunc(wanted, func(id int64) bool { return slices.Contains(held, id) }); k >= 0 {
				owner = wanted[k]
			}
			judge := selection.Context{Profile: profile, Show: show, Want: owner, Original: original, Missing: wanted}
			eval := selection.Evaluate(judge, selection.Release{
				Claims: claims, Match: m, IndexerLanguages: selection.IndexerLanguages(r.Languages),
				OtherShow: r.TVDBID != 0 && r.TVDBID != seriesID, Published: r.Published,
				Size: r.Size, Runtime: runtimeOf(d, m.Episodes),
			})
			hits = append(hits, hit{indexer: ix.Name, result: r, eval: eval, episodes: m.Episodes, owner: owner})
		}
	}

	evals := make([]selection.Evaluation, len(hits))
	for i, h := range hits {
		evals[i] = h.eval
	}
	sr.Candidates = []store.Candidate{}
	var owners []int64
	for _, i := range selection.Rank(evals) {
		sr.Candidates = append(sr.Candidates, hits[i].candidate())
		owners = append(owners, hits[i].owner)
	}
	sr.Outcome, sr.Summary = outcome(selection.PackSummary(profile, evals), evals, sr.Indexers)
	if err := s.store.SaveSeasonSearch(ctx, seriesID, sr, owners); err != nil {
		return store.Search{}, err
	}
	saved, err := s.store.LoadSeasonSearch(ctx, seriesID, season)
	if err != nil {
		return store.Search{}, err
	}
	s.log.Info("searched a season", "series", seriesID, "season", season, "outcome", saved.Outcome,
		"packs", len(saved.Candidates), "duration", s.now().Sub(start).Round(time.Millisecond))
	// No OnDone: a season search changes no episode's state, so clients
	// have nothing to reload; the caller gets the result itself.
	return saved, nil
}

// seasonEpisodes lists a season's episodes in the order's sequence.
func seasonEpisodes(d store.SeriesData, order string, season int) []int64 {
	var eps []store.Episode
	for _, e := range d.Episodes {
		if p, ok := e.Numbers[order]; ok && p.Season == season {
			eps = append(eps, e)
		}
	}
	slices.SortFunc(eps, func(a, b store.Episode) int {
		return cmp.Compare(a.Numbers[order].Number, b.Numbers[order].Number)
	})
	ids := make([]int64, len(eps))
	for i, e := range eps {
		ids[i] = e.ID
	}
	return ids
}

// missing keeps the episodes that have aired by now and have no file.
func missing(d store.SeriesData, episodes []int64, files map[int64]string, now time.Time) []int64 {
	aired := map[int64]bool{}
	today := now.Format(time.DateOnly)
	for _, e := range d.Episodes {
		aired[e.ID] = e.Aired != "" && e.Aired <= today
	}
	return slices.DeleteFunc(slices.Clone(episodes), func(id int64) bool {
		_, has := files[id]
		return has || !aired[id]
	})
}

// report passes a copy on, so the next answer cannot change what was sent.
func (s *Service) report(p Progress) {
	if s.OnProgress != nil {
		p.Indexers = slices.Clone(p.Indexers)
		s.OnProgress(p)
	}
}

// hit is one search result with its verdict.
type hit struct {
	indexer  string
	result   prowlarr.Result
	eval     selection.Evaluation
	episodes []int64
	owner    int64 // for a season search: the episode a download belongs to
}

func (h hit) candidate() store.Candidate {
	c := store.Candidate{
		Indexer: h.indexer, Title: h.result.Title, Link: h.result.Link, Size: h.result.Size, Grabs: h.result.Grabs,
		Verdict: string(h.eval.Verdict), Tier: h.eval.Tier, Reasons: h.eval.Reasons, Decisive: h.eval.Decisive,
		Episodes: h.episodes,
	}
	if !h.result.Published.IsZero() {
		p := h.result.Published
		c.Published = &p
	}
	for _, l := range h.eval.Audio {
		c.Audio = append(c.Audio, string(l))
	}
	if c.Episodes == nil {
		c.Episodes = []int64{}
	}
	return c
}

// outcome sums up a search: found when a release is accepted, fallback
// when only last resorts are left, failed when every indexer failed
// without returning anything. An indexer that failed on a later page still
// delivered the results before it.
func outcome(summary string, evals []selection.Evaluation, indexers []store.SearchIndexer) (string, string) {
	var failed []string
	for _, ix := range indexers {
		if ix.Error != "" {
			failed = append(failed, ix.Name)
		}
	}
	if len(failed) == len(indexers) && len(evals) == 0 {
		if len(indexers) == 0 {
			return "failed", "Search failed: Prowlarr has no enabled Usenet indexers"
		}
		return "failed", "Search failed: " + indexers[0].Error
	}
	if len(failed) > 0 {
		summary += fmt.Sprintf(" (%s failed)", strings.Join(failed, ", "))
	}
	result := "nothing"
	for _, e := range evals {
		switch {
		case e.Verdict == selection.Accept:
			return "found", summary
		case e.Verdict == selection.Fallback:
			result = "fallback"
		}
	}
	return result, summary
}

// runtimeOf is how long the episodes run, each by its own runtime or the
// show's average; 0 when TVDB knows neither.
func runtimeOf(d store.SeriesData, episodes []int64) time.Duration {
	var total time.Duration
	for _, id := range episodes {
		i := slices.IndexFunc(d.Episodes, func(e store.Episode) bool { return e.ID == id })
		minutes := d.Series.AverageRuntime
		if i >= 0 && d.Episodes[i].Runtime > 0 {
			minutes = d.Episodes[i].Runtime
		}
		if minutes <= 0 {
			return 0
		}
		total += time.Duration(minutes) * time.Minute
	}
	return total
}
