// Package prowlarr searches indexers through Prowlarr. Each indexer is
// queried on its own Newznab endpoint, because Prowlarr's combined search
// drops language attributes and hides failed indexers behind empty results.
package prowlarr

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trucke/garner/internal/httpx"
)

const (
	// pageSize is the most results indexers return per request.
	pageSize = 100
	// maxPages bounds paging when an indexer ignores the season and
	// episode and returns the whole show.
	maxPages = 10
	// requestGap keeps Garner under 5 requests per second, a limit some
	// indexers set, counting every request that goes through Prowlarr.
	requestGap = 250 * time.Millisecond
)

// Client talks to one Prowlarr instance. The API key travels in a header,
// so request URLs and the errors quoting them hold no credentials.
type Client struct {
	base string
	key  string
	http *http.Client
	// Now and Sleep space the requests out: the real clock, or the
	// simulation's.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	mu   sync.Mutex
	next time.Time // earliest start of the next request
}

// New returns a client for the Prowlarr at baseURL.
func New(baseURL, apiKey string, client *http.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), key: apiKey, http: client, Now: time.Now, Sleep: sleep}
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Indexer is an indexer configured in Prowlarr.
type Indexer struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enable"`
	Protocol string `json:"protocol"`
}

// Indexers lists the enabled Usenet indexers.
func (c *Client) Indexers(ctx context.Context) ([]Indexer, error) {
	body, err := c.get(ctx, "/api/v1/indexer")
	if err != nil {
		return nil, err
	}
	var all []Indexer
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("Prowlarr indexers: %w", err)
	}
	var out []Indexer
	for _, i := range all {
		if i.Enabled && i.Protocol == "usenet" {
			out = append(out, i)
		}
	}
	return out, nil
}

// Query is a TV search by TVDB ID. Episode 0 searches the whole season.
type Query struct {
	TVDBID  int64
	Season  int
	Episode int
}

// Result is one release an indexer found.
type Result struct {
	Title string
	// Link downloads the NZB through Prowlarr. It holds Prowlarr's API key:
	// never log it or return it to clients.
	Link string
	// Posting tells NZB postings apart that share the release name: a
	// fingerprint of the indexer and its ID for the posting. The link
	// cannot, as Prowlarr encrypts it anew for every search; the ID can be
	// a link with credentials, so only its fingerprint leaves this package.
	Posting string
	// Page is the release's page at the indexer, where people may have
	// commented on it; empty when its address could hold credentials.
	Page      string
	Size      int64
	Published time.Time
	Grabs     int
	// TVDBID is the show the indexer files the release under; 0 if unknown.
	TVDBID int64
	// Languages are the audio languages the indexer reports, as it writes
	// them ("German", "ja-JP").
	Languages []string
}

// Search queries one indexer and pages through its results. On an error it
// also returns the results of the pages before.
func (c *Client) Search(ctx context.Context, indexer int, q Query) ([]Result, error) {
	params := url.Values{
		"t": {"tvsearch"}, "tvdbid": {strconv.FormatInt(q.TVDBID, 10)}, "season": {strconv.Itoa(q.Season)},
		"extended": {"1"}, "limit": {strconv.Itoa(pageSize)},
	}
	if q.Episode > 0 {
		params.Set("ep", strconv.Itoa(q.Episode))
	}
	var out []Result
	for page := 0; page < maxPages; page++ {
		params.Set("offset", strconv.Itoa(page*pageSize))
		body, err := c.get(ctx, fmt.Sprintf("/%d/api?%s", indexer, params.Encode()))
		if err != nil {
			return out, err
		}
		results, err := parseFeed(body, indexer)
		if err != nil {
			return out, err
		}
		out = append(out, results...)
		if len(results) < pageSize {
			break
		}
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, httpx.Redact(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, httpx.Redact(err)
	}
	// Newznab reports failures as an <error> document, sometimes with 200.
	if msg := newznabError(body); msg != "" {
		return nil, errors.New(msg)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prowlarr answered %s", resp.Status)
	}
	return body, nil
}

// wait spaces requests out by requestGap.
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := c.Now()
	start := c.next
	if now.After(start) {
		start = now
	}
	c.next = start.Add(requestGap)
	c.mu.Unlock()
	if d := start.Sub(now); d > 0 {
		return c.Sleep(ctx, d)
	}
	return nil
}

type feed struct {
	Items []struct {
		Title    string `xml:"title"`
		GUID     string `xml:"guid"`
		Comments string `xml:"comments"`
		Link     string `xml:"link"`
		Size     int64  `xml:"size"`
		PubDate  string `xml:"pubDate"`
		Attrs    []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"http://www.newznab.com/DTD/2010/feeds/attributes/ attr"`
	} `xml:"channel>item"`
}

func parseFeed(body []byte, indexer int) ([]Result, error) {
	var f feed
	if err := xml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("unreadable indexer response: %w", err)
	}
	out := make([]Result, 0, len(f.Items))
	for _, it := range f.Items {
		r := Result{Title: strings.TrimSpace(it.Title), Link: it.Link, Size: it.Size}
		r.Published, _ = time.Parse(time.RFC1123Z, it.PubDate)
		for _, a := range it.Attrs {
			switch a.Name {
			case "language":
				if v := strings.TrimSpace(a.Value); v != "" {
					r.Languages = append(r.Languages, v)
				}
			case "grabs":
				r.Grabs, _ = strconv.Atoi(a.Value)
			case "tvdbid":
				r.TVDBID, _ = strconv.ParseInt(a.Value, 10, 64)
			case "size":
				if r.Size == 0 {
					r.Size, _ = strconv.ParseInt(a.Value, 10, 64)
				}
			}
		}
		// Without the indexer's ID, the name, size and time stand in.
		id := strings.TrimSpace(it.GUID)
		if id == "" {
			id = fmt.Sprintf("%s|%d|%d", r.Title, r.Size, r.Published.Unix())
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "%d\n%s", indexer, id))
		r.Posting = hex.EncodeToString(sum[:8])
		r.Page = cmp.Or(page(strings.TrimSpace(it.Comments)), page(id))
		if r.Title != "" && r.Link != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// page returns the address of a release's page at the indexer, or "" when
// it could hold credentials, in its query or its path. Only the two shapes
// indexers give these pages pass: /details/<id>, and a single page that
// only the release's guid or id names, as in /geekseek.php?guid=<id>.
func page(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	path := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(query) == 0 && len(path) == 2 && path[0] == "details" && path[1] != "":
	case len(query) == 1 && len(path) == 1 && path[0] != "" && (len(query["guid"]) == 1 || len(query["id"]) == 1):
	default:
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

func newznabError(body []byte) string {
	var e struct {
		XMLName     xml.Name `xml:"error"`
		Code        string   `xml:"code,attr"`
		Description string   `xml:"description,attr"`
	}
	if xml.Unmarshal(body, &e) != nil || e.XMLName.Local != "error" {
		return ""
	}
	return fmt.Sprintf("%s (Newznab error %s)", e.Description, e.Code)
}
