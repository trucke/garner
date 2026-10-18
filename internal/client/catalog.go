package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/trucke/garner/internal/catalog"
	"github.com/trucke/garner/internal/server"
	"github.com/trucke/garner/internal/store"
)

// Library lists the shows with a summary of each.
func (c *Client) Library(ctx context.Context) ([]store.Show, error) {
	var shows []store.Show
	return shows, c.do(ctx, http.MethodGet, "/api/v1/series", nil, &shows)
}

// Search finds shows on TVDB. q may also be a TVDB ID or a thetvdb.com link.
func (c *Client) Search(ctx context.Context, q string) (catalog.SearchPage, error) {
	var page catalog.SearchPage
	return page, c.do(ctx, http.MethodGet, "/api/v1/search?"+url.Values{"q": {q}}.Encode(), nil, &page)
}

// Preview describes a show on TVDB before it is added.
func (c *Client) Preview(ctx context.Context, id int64) (catalog.Preview, error) {
	var p catalog.Preview
	return p, c.do(ctx, http.MethodGet, "/api/v1/search/"+strconv.FormatInt(id, 10), nil, &p)
}

// Add loads a show into the library; monitor says which of its episodes
// the monitor gets. It takes as long as loading the show's episodes from
// TVDB and TheXEM does, usually a few seconds.
func (c *Client) Add(ctx context.Context, id int64, monitor string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/series", map[string]any{"tvdb_id": id, "monitor": monitor}, nil)
}

// Series returns a show with what Garner has of each episode.
func (c *Client) Series(ctx context.Context, id int64) (server.SeriesDetail, error) {
	var d server.SeriesDetail
	return d, c.do(ctx, http.MethodGet, "/api/v1/series/"+strconv.FormatInt(id, 10), nil, &d)
}

// SetProfile changes a show's profile. Its earlier search results are
// cleared.
func (c *Client) SetProfile(ctx context.Context, id int64, profile string) (server.SeriesDetail, error) {
	var d server.SeriesDetail
	return d, c.do(ctx, http.MethodPatch, "/api/v1/series/"+strconv.FormatInt(id, 10), map[string]string{"profile": profile}, &d)
}

// SetMonitor changes which of a show's episodes the monitor gets.
func (c *Client) SetMonitor(ctx context.Context, id int64, mode string) (server.SeriesDetail, error) {
	var d server.SeriesDetail
	return d, c.do(ctx, http.MethodPatch, "/api/v1/series/"+strconv.FormatInt(id, 10), map[string]string{"monitor": mode}, &d)
}

// SearchShow asks the server to search every missing episode of a show
// and download the best release of each that meets its rules. It returns
// how many episodes are missing; series.search events follow the search.
func (c *Client) SearchShow(ctx context.Context, id int64) (int, error) {
	var out struct {
		Episodes int `json:"episodes"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/series/"+strconv.FormatInt(id, 10)+"/search", nil, &out)
	return out.Episodes, err
}

// Refresh reloads a show from TVDB and TheXEM.
func (c *Client) Refresh(ctx context.Context, id int64) (server.SeriesDetail, error) {
	var d server.SeriesDetail
	return d, c.do(ctx, http.MethodPost, "/api/v1/series/"+strconv.FormatInt(id, 10)+"/refresh", nil, &d)
}

// Deleted is what deleting a show removed, or kept.
type Deleted struct {
	Files int   `json:"files"` // deleted
	Kept  int   `json:"kept"`  // left in the library
	Bytes int64 `json:"bytes"`
	// Problems lists files that could not be removed; the show is gone
	// regardless.
	Problems []string `json:"problems"`
}

// DeleteSeries removes a show and cancels its downloads. Its files are
// deleted unless keepFiles is set.
func (c *Client) DeleteSeries(ctx context.Context, id int64, keepFiles bool) (Deleted, error) {
	files := "delete"
	if keepFiles {
		files = "keep"
	}
	var d Deleted
	return d, c.do(ctx, http.MethodDelete, "/api/v1/series/"+strconv.FormatInt(id, 10)+"?files="+files, nil, &d)
}

// SearchEpisode searches the indexers for an episode.
func (c *Client) SearchEpisode(ctx context.Context, episodeID int64) (store.Search, error) {
	var s store.Search
	return s, c.do(ctx, http.MethodPost, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10)+"/search", nil, &s)
}

// EpisodeSearch returns an episode's latest stored search.
func (c *Client) EpisodeSearch(ctx context.Context, episodeID int64) (store.Search, error) {
	var s store.Search
	return s, c.do(ctx, http.MethodGet, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10)+"/search", nil, &s)
}

// DownloadCandidate downloads a release of a search, whatever its verdict.
func (c *Client) DownloadCandidate(ctx context.Context, candidateID int64) (store.Download, error) {
	var d store.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/candidates/"+strconv.FormatInt(candidateID, 10)+"/download", nil, &d)
}

// Episode returns an episode's file and downloads.
func (c *Client) Episode(ctx context.Context, episodeID int64) (server.EpisodeDetail, error) {
	var e server.EpisodeDetail
	return e, c.do(ctx, http.MethodGet, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10), nil, &e)
}

// CancelDownload stops a queued or downloading release.
func (c *Client) CancelDownload(ctx context.Context, downloadID int64) (store.Download, error) {
	var d store.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/downloads/"+strconv.FormatInt(downloadID, 10)+"/cancel", nil, &d)
}

// RetryImport repeats a failed or unconfirmed import.
func (c *Client) RetryImport(ctx context.Context, downloadID int64) (store.Download, error) {
	var d store.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/downloads/"+strconv.FormatInt(downloadID, 10)+"/import", nil, &d)
}

// SearchSeason searches a whole season for packs.
func (c *Client) SearchSeason(ctx context.Context, seriesID int64, season int) (store.Search, error) {
	var s store.Search
	return s, c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/series/%d/seasons/%d/search", seriesID, season), nil, &s)
}

// DownloadSeasonCandidate downloads a pack of a season search.
func (c *Client) DownloadSeasonCandidate(ctx context.Context, candidateID int64) (store.Download, error) {
	var d store.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/season-candidates/"+strconv.FormatInt(candidateID, 10)+"/download", nil, &d)
}

// Services asks the server how the services it uses are doing, and for the
// library's totals. The server checks each service live.
func (c *Client) Services(ctx context.Context) (server.Services, error) {
	var s server.Services
	return s, c.do(ctx, http.MethodGet, "/api/v1/status/services", nil, &s)
}

// Activity lists what runs, what waits for the user, and what ended within
// the last day.
func (c *Client) Activity(ctx context.Context) ([]store.ActivityItem, error) {
	var items []store.ActivityItem
	return items, c.do(ctx, http.MethodGet, "/api/v1/activity", nil, &items)
}
