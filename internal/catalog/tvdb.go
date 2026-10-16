package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// TVDBBaseURL is TheTVDB's v4 API.
const TVDBBaseURL = "https://api4.thetvdb.com/v4"

// errNotFound means TVDB has no such record, such as a missing translation.
var errNotFound = errors.New("not found on TVDB")

// tokenMaxAge renews the login token well before TVDB's one-month expiry.
const tokenMaxAge = 24 * time.Hour

// TVDB is a client for TheTVDB's v4 API.
type TVDB struct {
	base   string
	apiKey string
	pin    string
	http   *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// NewTVDB returns a client. pin is the subscriber PIN and may be empty.
func NewTVDB(base, apiKey, pin string, client *http.Client) *TVDB {
	return &TVDB{base: base, apiKey: apiKey, pin: pin, http: client}
}

type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Links   struct {
		Next       *string `json:"next"`
		TotalItems int     `json:"total_items"`
	} `json:"links"`
}

func (c *TVDB) login(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < tokenMaxAge {
		return c.token, nil
	}
	body := map[string]string{"apikey": c.apiKey}
	if c.pin != "" {
		body["pin"] = c.pin
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/login", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("TVDB login: %w", err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TVDB login failed (%s): %s", resp.Status, env.Message)
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Token == "" {
		return "", errors.New("TVDB login returned no token")
	}
	c.token, c.tokenAt = data.Token, time.Now()
	return c.token, nil
}

func (c *TVDB) forgetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// get fetches path and decodes the envelope's data into out. It logs in
// again once if TVDB rejects the token.
func (c *TVDB) get(ctx context.Context, path string, out any) (*envelope, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.login(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("TVDB %s: %w", path, err)
		}
		var env envelope
		decodeErr := json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c.forgetToken()
			continue
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("TVDB %s: %s %s", path, resp.Status, env.Message)
		case decodeErr != nil:
			return nil, fmt.Errorf("TVDB %s: %w", path, decodeErr)
		}
		if out != nil {
			if err := json.Unmarshal(env.Data, out); err != nil {
				return nil, fmt.Errorf("TVDB %s: %w", path, err)
			}
		}
		return &env, nil
	}
}

// tvdbSearchResult is one series from /search.
type tvdbSearchResult struct {
	TVDBID          string            `json:"tvdb_id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Year            string            `json:"year"`
	Status          string            `json:"status"`
	PrimaryLanguage string            `json:"primary_language"`
	Network         string            `json:"network"`
	Translations    map[string]string `json:"translations"`
}

// search returns up to searchLimit series and how many TVDB found in all.
func (c *TVDB) search(ctx context.Context, query string) ([]tvdbSearchResult, int, error) {
	q := url.Values{"query": {query}, "type": {"series"}, "limit": {strconv.Itoa(searchLimit)}}
	var out []tvdbSearchResult
	env, err := c.get(ctx, "/search?"+q.Encode(), &out)
	if err != nil {
		return nil, 0, err
	}
	return out, max(env.Links.TotalItems, len(out)), nil
}

const searchLimit = 50

// seriesIDBySlug resolves a slug from a thetvdb.com link.
func (c *TVDB) seriesIDBySlug(ctx context.Context, slug string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	_, err := c.get(ctx, "/series/slug/"+url.PathEscape(slug), &out)
	return out.ID, err
}

type tvdbSeasonType struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	AlternateName *string `json:"alternateName"`
}

type tvdbSeries struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	OriginalLanguage string `json:"originalLanguage"`
	OriginalCountry  string `json:"originalCountry"`
	Year             string `json:"year"`
	FirstAired       string `json:"firstAired"`
	LastAired        string `json:"lastAired"`
	Overview         string `json:"overview"`
	AverageRuntime   int    `json:"averageRuntime"`
	OriginalNetwork  *struct {
		Name string `json:"name"`
	} `json:"originalNetwork"`
	DefaultSeason int    `json:"defaultSeasonType"`
	LastUpdated   string `json:"lastUpdated"`
	Status        struct {
		Name string `json:"name"`
	} `json:"status"`
	Aliases []struct {
		Language string `json:"language"`
		Name     string `json:"name"`
	} `json:"aliases"`
	SeasonTypes []tvdbSeasonType `json:"seasonTypes"`
	Genres      []struct {
		Name string `json:"name"`
	} `json:"genres"`
}

func (c *TVDB) series(ctx context.Context, id int64) (tvdbSeries, error) {
	var out tvdbSeries
	_, err := c.get(ctx, fmt.Sprintf("/series/%d/extended?short=true", id), &out)
	return out, err
}

type tvdbTranslation struct {
	Name     string   `json:"name"`
	Overview string   `json:"overview"`
	Aliases  []string `json:"aliases"`
}

// translation returns the series' name in lang, or found=false.
func (c *TVDB) translation(ctx context.Context, id int64, lang string) (tvdbTranslation, bool, error) {
	var out tvdbTranslation
	_, err := c.get(ctx, fmt.Sprintf("/series/%d/translations/%s", id, lang), &out)
	if errors.Is(err, errNotFound) {
		return out, false, nil
	}
	return out, err == nil, err
}

type tvdbEpisode struct {
	ID             int64   `json:"id"`
	SeasonNumber   int     `json:"seasonNumber"`
	Number         int     `json:"number"`
	Name           *string `json:"name"`
	Aired          *string `json:"aired"`
	Runtime        *int    `json:"runtime"`
	Overview       *string `json:"overview"`
	AbsoluteNumber int     `json:"absoluteNumber"`
	IsMovie        int     `json:"isMovie"`
}

// episodes lists a series' episodes in one season type (order). With lang
// set, names are in that language and null where TVDB has no translation.
// A missing language yields found=false.
func (c *TVDB) episodes(ctx context.Context, id int64, seasonType, lang string) ([]tvdbEpisode, bool, error) {
	var all []tvdbEpisode
	for page := 0; page < 100; page++ {
		path := fmt.Sprintf("/series/%d/episodes/%s", id, url.PathEscape(seasonType))
		if lang != "" {
			path += "/" + lang
		}
		var data struct {
			Episodes []tvdbEpisode `json:"episodes"`
		}
		env, err := c.get(ctx, path+"?page="+strconv.Itoa(page), &data)
		if errors.Is(err, errNotFound) && lang != "" {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		all = append(all, data.Episodes...)
		if env.Links.Next == nil || *env.Links.Next == "" {
			return all, true, nil
		}
	}
	return nil, false, fmt.Errorf("TVDB series %d: too many episode pages", id)
}

// updatedSeries returns the IDs of series changed since t.
func (c *TVDB) updatedSeries(ctx context.Context, since time.Time) ([]int64, error) {
	var ids []int64
	for page := 0; page < 1000; page++ {
		q := url.Values{"since": {strconv.FormatInt(since.Unix(), 10)}, "type": {"series"}, "page": {strconv.Itoa(page)}}
		var data []struct {
			RecordID int64 `json:"recordId"`
		}
		env, err := c.get(ctx, "/updates?"+q.Encode(), &data)
		if err != nil {
			return nil, err
		}
		for _, d := range data {
			ids = append(ids, d.RecordID)
		}
		if env.Links.Next == nil || *env.Links.Next == "" {
			return ids, nil
		}
	}
	return nil, errors.New("TVDB updates: too many pages")
}
