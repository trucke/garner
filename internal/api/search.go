package api

import "time"

// Search is the latest search for an episode.
type Search struct {
	EpisodeID int64 `json:"episode_id"`
	// Season is set instead for a season search, which keeps only packs.
	Season   int       `json:"season,omitempty"`
	Searched time.Time `json:"searched"`
	Outcome  string    `json:"outcome"` // found, fallback, nothing or failed
	Summary  string    `json:"summary"`
	// NextRetry is when the monitor searches the episode again, as the
	// server answers; unset when the monitor does not get the episode.
	NextRetry  *time.Time      `json:"next_retry,omitempty"`
	Indexers   []SearchIndexer `json:"indexers"`
	Candidates []Candidate     `json:"candidates"`
}

// SearchIndexer is what one indexer returned.
type SearchIndexer struct {
	Name    string `json:"name"`
	Results int    `json:"results"`
	Error   string `json:"error,omitempty"`
}

// Candidate is a release a search found, with the verdict on it.
type Candidate struct {
	ID   int64 `json:"id"`
	Rank int   `json:"rank"`
	// SearchSeason identifies the parent regular-season search when its
	// download link is loaded; zero means an episode search. Specials
	// have no season-pack searches. Never sent to clients.
	SearchSeason int    `json:"-"`
	Indexer      string `json:"indexer"`
	Title        string `json:"title"`
	// Link downloads the release through Prowlarr and holds its API key. It
	// is stored but never loaded for or sent to clients.
	Link string `json:"-"`
	// Posting tells NZB postings of the same release apart: a fingerprint
	// of the indexer's ID for this one. Empty for candidates stored before
	// Garner kept it. Never sent to clients.
	Posting string `json:"-"`
	// Page is the release's page at the indexer, for people to open; empty
	// when unknown or when its address could hold credentials.
	Page      string     `json:"page,omitempty"`
	Size      int64      `json:"size"`
	Published *time.Time `json:"published,omitempty"`
	Grabs     int        `json:"grabs"`
	Verdict   string     `json:"verdict"`
	Tier      string     `json:"tier,omitempty"`
	Audio     []string   `json:"audio"`
	Reasons   []string   `json:"reasons"`
	// Decisive counts the leading reasons that decided the verdict: why it
	// was rejected, or why it is only a last resort. The rest are facts.
	Decisive int     `json:"decisive"`
	Episodes []int64 `json:"episodes"`
	// Attempt is the latest download of this posting for the episode the
	// candidate is for; nil when Garner never tried it.
	Attempt *Download `json:"attempt,omitempty"`
}

// SamePosting reports whether the download is of this candidate's NZB
// posting: by the indexer's ID for it when both have one, otherwise, for
// rows from before Garner kept it, by name and size.
func (c Candidate) SamePosting(d Download) bool {
	if c.Posting != "" && d.Posting != "" {
		return c.Posting == d.Posting
	}
	return c.Title == d.Title && c.Size == d.Size
}

// SearchProgress is a running search: every indexer, and what it answered
// so far. A season search sets SeriesID and Season instead of EpisodeID.
type SearchProgress struct {
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

// ShowProgress is a search of every missing episode of a show.
type ShowProgress struct {
	SeriesID int64 `json:"series_id"`
	// Waiting is set until the search starts, after the searches before it.
	Waiting bool `json:"waiting,omitempty"`
	// Total counts the episodes to search, Done those searched so far, and
	// Downloads the downloads they started.
	Total     int  `json:"total"`
	Done      int  `json:"done"`
	Downloads int  `json:"downloads"`
	Finished  bool `json:"finished,omitempty"`
}
