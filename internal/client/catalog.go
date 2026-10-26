package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/trucke/garner/internal/api"
)

// Library lists the shows with a summary of each.
func (c *Client) Library(ctx context.Context) ([]api.Show, error) {
	var shows []api.Show
	return shows, c.do(ctx, http.MethodGet, "/api/v1/series", nil, &shows)
}

// Search finds shows on TVDB. q may also be a TVDB ID or a thetvdb.com link.
func (c *Client) Search(ctx context.Context, q string) (api.SearchPage, error) {
	var page api.SearchPage
	return page, c.do(ctx, http.MethodGet, "/api/v1/search?"+url.Values{"q": {q}}.Encode(), nil, &page)
}

// Preview describes a show on TVDB before it is added.
func (c *Client) Preview(ctx context.Context, id int64) (api.Preview, error) {
	var p api.Preview
	return p, c.do(ctx, http.MethodGet, "/api/v1/search/"+strconv.FormatInt(id, 10), nil, &p)
}

// Add loads a show into the library and takes over its files there;
// monitor says which of its episodes the monitor gets. It takes as long as
// loading the show's episodes from TVDB and TheXEM does, usually a few
// seconds, plus checking the files found, which can take minutes.
func (c *Client) Add(ctx context.Context, id int64, monitor string) (api.SeriesDetail, error) {
	var d api.SeriesDetail
	return d, c.do(ctx, http.MethodPost, "/api/v1/series", map[string]any{"tvdb_id": id, "monitor": monitor}, &d)
}

// TakeOver looks for a show's files in the library and takes over those of
// episodes without a file.
func (c *Client) TakeOver(ctx context.Context, id int64) (api.TakeOver, error) {
	var t api.TakeOver
	return t, c.do(ctx, http.MethodPost, "/api/v1/series/"+strconv.FormatInt(id, 10)+"/files/take-over", nil, &t)
}

// Rename moves a show's taken-over files into Garner's folder for it,
// named the way Garner names files.
func (c *Client) Rename(ctx context.Context, id int64) (api.Renamed, error) {
	var r api.Renamed
	return r, c.do(ctx, http.MethodPost, "/api/v1/series/"+strconv.FormatInt(id, 10)+"/files/rename", nil, &r)
}

// Series returns a show with what Garner has of each episode.
func (c *Client) Series(ctx context.Context, id int64) (api.SeriesDetail, error) {
	var d api.SeriesDetail
	return d, c.do(ctx, http.MethodGet, "/api/v1/series/"+strconv.FormatInt(id, 10), nil, &d)
}

// SetProfile gives a show another profile. Its earlier search results are
// cleared.
func (c *Client) SetProfile(ctx context.Context, id, profileID int64) (api.SeriesDetail, error) {
	var d api.SeriesDetail
	return d, c.do(ctx, http.MethodPatch, "/api/v1/series/"+strconv.FormatInt(id, 10), map[string]int64{"profile_id": profileID}, &d)
}

// SetMonitor changes which of a show's episodes the monitor gets.
func (c *Client) SetMonitor(ctx context.Context, id int64, mode string) (api.SeriesDetail, error) {
	var d api.SeriesDetail
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
func (c *Client) Refresh(ctx context.Context, id int64) (api.SeriesDetail, error) {
	var d api.SeriesDetail
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
func (c *Client) SearchEpisode(ctx context.Context, episodeID int64) (api.Search, error) {
	var s api.Search
	return s, c.do(ctx, http.MethodPost, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10)+"/search", nil, &s)
}

// EpisodeSearch returns an episode's latest stored search.
func (c *Client) EpisodeSearch(ctx context.Context, episodeID int64) (api.Search, error) {
	var s api.Search
	return s, c.do(ctx, http.MethodGet, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10)+"/search", nil, &s)
}

// DownloadCandidate downloads a release of a search, whatever its verdict.
func (c *Client) DownloadCandidate(ctx context.Context, candidateID int64) (api.Download, error) {
	var d api.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/candidates/"+strconv.FormatInt(candidateID, 10)+"/download", nil, &d)
}

// Episode returns an episode's file and downloads.
func (c *Client) Episode(ctx context.Context, episodeID int64) (api.EpisodeDetail, error) {
	var e api.EpisodeDetail
	return e, c.do(ctx, http.MethodGet, "/api/v1/episodes/"+strconv.FormatInt(episodeID, 10), nil, &e)
}

// CancelDownload stops a queued or downloading release.
func (c *Client) CancelDownload(ctx context.Context, downloadID int64) (api.Download, error) {
	var d api.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/downloads/"+strconv.FormatInt(downloadID, 10)+"/cancel", nil, &d)
}

// AcknowledgeDownload clears a failed attempt's warning without retrying it.
func (c *Client) AcknowledgeDownload(ctx context.Context, downloadID int64) (api.Download, error) {
	var d api.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/downloads/"+strconv.FormatInt(downloadID, 10)+"/acknowledge", nil, &d)
}

// RetryImport repeats a failed or unconfirmed import.
func (c *Client) RetryImport(ctx context.Context, downloadID int64) (api.Download, error) {
	var d api.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/downloads/"+strconv.FormatInt(downloadID, 10)+"/import", nil, &d)
}

// SearchSeason searches a whole season for packs.
func (c *Client) SearchSeason(ctx context.Context, seriesID int64, season int) (api.Search, error) {
	var s api.Search
	return s, c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/series/%d/seasons/%d/search", seriesID, season), nil, &s)
}

// DownloadSeasonCandidate downloads a pack of a season search.
func (c *Client) DownloadSeasonCandidate(ctx context.Context, candidateID int64) (api.Download, error) {
	var d api.Download
	return d, c.do(ctx, http.MethodPost, "/api/v1/season-candidates/"+strconv.FormatInt(candidateID, 10)+"/download", nil, &d)
}

// Services asks the server how the services it uses are doing, and for the
// library's totals. The server checks each service live.
func (c *Client) Services(ctx context.Context) (api.Services, error) {
	var s api.Services
	return s, c.do(ctx, http.MethodGet, "/api/v1/status/services", nil, &s)
}

// Activity lists what runs, what waits for the user, and what ended within
// the last day.
func (c *Client) Activity(ctx context.Context) ([]api.ActivityItem, error) {
	var items []api.ActivityItem
	return items, c.do(ctx, http.MethodGet, "/api/v1/activity", nil, &items)
}
