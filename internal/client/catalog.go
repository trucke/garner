package client

import (
	"context"
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

// Add loads a show into the library. It takes as long as loading the show's
// episodes from TVDB and TheXEM does, usually a few seconds.
func (c *Client) Add(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, "/api/v1/series", map[string]int64{"tvdb_id": id}, nil)
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

// Refresh reloads a show from TVDB and TheXEM.
func (c *Client) Refresh(ctx context.Context, id int64) (server.SeriesDetail, error) {
	var d server.SeriesDetail
	return d, c.do(ctx, http.MethodPost, "/api/v1/series/"+strconv.FormatInt(id, 10)+"/refresh", nil, &d)
}

// Deleted is what deleting a show removed.
type Deleted struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Problems lists files that could not be removed; the show is gone
	// regardless.
	Problems []string `json:"problems"`
}

// DeleteSeries removes a show, cancels its downloads and deletes its files.
func (c *Client) DeleteSeries(ctx context.Context, id int64) (Deleted, error) {
	var d Deleted
	return d, c.do(ctx, http.MethodDelete, "/api/v1/series/"+strconv.FormatInt(id, 10), nil, &d)
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
