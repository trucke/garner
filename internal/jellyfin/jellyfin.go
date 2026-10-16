// Package jellyfin tells Jellyfin about imported files and reads how it
// identified them.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/trucke/garner/internal/httpx"
)

// Client talks to one Jellyfin server. The API key travels in the
// Authorization header.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client for the Jellyfin at baseURL.
func New(baseURL, apiKey string, client *http.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: apiKey, http: client}
}

// Created tells Jellyfin that files appeared, so it scans just those paths.
// Paths are as Jellyfin sees them.
func (c *Client) Created(ctx context.Context, paths []string) error {
	return c.updated(ctx, "Created", paths)
}

// Deleted tells Jellyfin that files are gone, so it drops them about a
// minute later instead of at its next library scan. Paths are as Jellyfin
// sees them.
func (c *Client) Deleted(ctx context.Context, paths []string) error {
	return c.updated(ctx, "Deleted", paths)
}

func (c *Client) updated(ctx context.Context, kind string, paths []string) error {
	type update struct {
		Path       string `json:"Path"`
		UpdateType string `json:"UpdateType"`
	}
	body := struct {
		Updates []update `json:"Updates"`
	}{}
	for _, p := range paths {
		body.Updates = append(body.Updates, update{Path: p, UpdateType: kind})
	}
	return c.do(ctx, http.MethodPost, "/Library/Media/Updated", body, nil)
}

// RefreshLibrary asks Jellyfin to scan all libraries. It is the fallback
// for libraries Jellyfin has never seen files in: it skips empty library
// folders, so it ignores Created for them.
func (c *Client) RefreshLibrary(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/Library/Refresh", nil, nil)
}

// Episode is how Jellyfin identified a file.
type Episode struct {
	Name       string
	Season     int
	Number     int
	TVDB       string // TVDB episode ID, empty if Jellyfin found none
	SeriesTVDB string
}

type item struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	Path              string            `json:"Path"`
	IndexNumber       int               `json:"IndexNumber"`
	ParentIndexNumber int               `json:"ParentIndexNumber"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
}

// EpisodeAt finds the episode Jellyfin made of a file. Jellyfin cannot
// search by path, so this finds the show by its folder first. found is
// false while Jellyfin has not listed the file yet.
func (c *Client) EpisodeAt(ctx context.Context, showDir, file string) (ep Episode, found bool, err error) {
	var shows struct {
		Items []item `json:"Items"`
	}
	q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Series"}, "Fields": {"Path,ProviderIds"}}
	if err := c.do(ctx, http.MethodGet, "/Items?"+q.Encode(), nil, &shows); err != nil {
		return ep, false, err
	}
	for _, s := range shows.Items {
		if s.Path != showDir {
			continue
		}
		var episodes struct {
			Items []item `json:"Items"`
		}
		path := "/Shows/" + url.PathEscape(s.ID) + "/Episodes?Fields=Path,ProviderIds"
		if err := c.do(ctx, http.MethodGet, path, nil, &episodes); err != nil {
			return ep, false, err
		}
		for _, e := range episodes.Items {
			if e.Path == file {
				return Episode{
					Name: e.Name, Season: e.ParentIndexNumber, Number: e.IndexNumber,
					TVDB: e.ProviderIDs["Tvdb"], SeriesTVDB: s.ProviderIDs["Tvdb"],
				}, true, nil
			}
		}
	}
	return ep, false, nil
}

// Info reports Jellyfin's version and how many shows its libraries hold.
func (c *Client) Info(ctx context.Context) (version string, shows int, err error) {
	var info struct{ Version string }
	if err := c.do(ctx, http.MethodGet, "/System/Info/Public", nil, &info); err != nil {
		return "", 0, err
	}
	var items struct{ TotalRecordCount int }
	if err := c.do(ctx, http.MethodGet, "/Items?IncludeItemTypes=Series&Recursive=true&Limit=0", nil, &items); err != nil {
		return info.Version, 0, err
	}
	return info.Version, items.TotalRecordCount, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+c.key+`"`)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return httpx.Redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Jellyfin refused the API key (%s)", resp.Status)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Jellyfin answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("unreadable Jellyfin response: %w", err)
	}
	return nil
}
