package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/trucke/garner/internal/catalog"
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
