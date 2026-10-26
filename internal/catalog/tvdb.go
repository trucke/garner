package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TVDBBaseURL is TheTVDB's v4 API.
const TVDBBaseURL = "https://api4.thetvdb.com/v4"

// errNotFound means TVDB has no such record, such as a missing translation.
var errNotFound = errors.New("not found on TVDB")

// tokenMaxAge renews the login token well before TVDB's one-month expiry.
const tokenMaxAge = 24 * time.Hour

// Limits on TheTVDB requests. Every installation of a release shares
// Garner's project key, so one that goes wrong, such as through a bug that
// reloads shows in a loop, must not get the key blocked for everyone.
const (
	// tvdbInterval paces requests: at most five per second.
	tvdbInterval = 200 * time.Millisecond
	// tvdbBudget is the most requests in tvdbWindow. Loading a show takes
	// three or four, so even taking over a large library stays far below.
	tvdbBudget = 10_000
	tvdbWindow = 24 * time.Hour
	// tvdbBackoff holds every request back after TheTVDB answers 429 or
	// 503 without saying for how long; tvdbMaxBackoff caps what it asks.
	tvdbBackoff    = 15 * time.Minute
	tvdbMaxBackoff = 24 * time.Hour
	// tvdbRefusedPause holds requests back after TheTVDB refuses the key,
	// so a revoked key does not make every installation log in again with
	// every request.
	tvdbRefusedPause = time.Hour
)

// PausedError means Garner holds its TheTVDB requests back until Until.
type PausedError struct {
	Until  time.Time
	Reason string
}

func (e *PausedError) Error() string {
	return fmt.Sprintf("TVDB requests paused until %s: %s", e.Until.Format("Mon 15:04"), e.Reason)
}

// tvdbMaxResponse bounds each TVDB response body. The largest answers
// Garner reads are extended series records with aliases, season types
// and genres, and pages of 100 episodes from very long-running shows;
// real ones stay a few hundred KiB, so 16 MiB leaves generous headroom
// without cutting off a valid show.
const tvdbMaxResponse = 16 << 20

// readBounded reads at most limit bytes plus one and reports whether the
// body exceeded the limit, so a broken or hostile response fails with a
// clear error instead of exhausting memory or an unexpected EOF deep in
// the decoder.
func readBounded(r io.Reader, limit int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	return b, int64(len(b)) > limit, nil
}

// TVDB is a client for TheTVDB's v4 API, within the limits above.
type TVDB struct {
	base   string
	apiKey string
	pin    string
	http   *http.Client

	// Now tells the time, and Sleep waits on the same clock; the
	// simulation replaces both.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	token   string
	tokenAt time.Time

	lim         sync.Mutex
	next        time.Time // when the next request may start
	windowStart time.Time
	used        int // requests since windowStart
	pausedUntil time.Time
	pauseReason string
}

// NewTVDB returns a client. pin is the subscriber PIN and may be empty.
func NewTVDB(base, apiKey, pin string, client *http.Client) *TVDB {
	return &TVDB{base: base, apiKey: apiKey, pin: pin, http: client, Now: time.Now, Sleep: sleep}
}

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

// reserve admits one request. It fails while requests are held back or
// the budget is spent, and otherwise waits for the request's turn.
func (c *TVDB) reserve(ctx context.Context) error {
	c.lim.Lock()
	now := c.Now()
	if now.Before(c.pausedUntil) {
		err := &PausedError{Until: c.pausedUntil, Reason: c.pauseReason}
		c.lim.Unlock()
		return err
	}
	if now.Sub(c.windowStart) >= tvdbWindow {
		c.windowStart, c.used = now, 0
	}
	if c.used >= tvdbBudget {
		c.pausedUntil = c.windowStart.Add(tvdbWindow)
		c.pauseReason = fmt.Sprintf("Garner sent TVDB %d requests within %s, its limit; this points to a bug in Garner, please report it",
			tvdbBudget, tvdbWindow)
		err := &PausedError{Until: c.pausedUntil, Reason: c.pauseReason}
		c.lim.Unlock()
		return err
	}
	c.used++
	start := now
	if c.next.After(now) {
		start = c.next
	}
	c.next = start.Add(tvdbInterval)
	c.lim.Unlock()
	if wait := start.Sub(now); wait > 0 {
		return c.Sleep(ctx, wait)
	}
	return nil
}

// pause holds every request back until until. A longer pause in place
// stays.
func (c *TVDB) pause(until time.Time, reason string) *PausedError {
	c.lim.Lock()
	defer c.lim.Unlock()
	if until.After(c.pausedUntil) {
		c.pausedUntil, c.pauseReason = until, reason
	}
	return &PausedError{Until: c.pausedUntil, Reason: c.pauseReason}
}

// Paused reports until when and why requests are held back; the zero time
// when they are not.
func (c *TVDB) Paused() (time.Time, string) {
	c.lim.Lock()
	defer c.lim.Unlock()
	if c.Now().Before(c.pausedUntil) {
		return c.pausedUntil, c.pauseReason
	}
	return time.Time{}, ""
}

// slowDown handles TheTVDB's 429 and 503: it holds every request back for
// as long as Retry-After asks, in seconds or as a date, up to
// tvdbMaxBackoff, or for tvdbBackoff when it does not say.
func (c *TVDB) slowDown(resp *http.Response) *PausedError {
	now := c.Now()
	wait := tvdbBackoff
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		wait = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(v); err == nil && t.After(now) {
		wait = t.Sub(now)
	}
	return c.pause(now.Add(min(wait, tvdbMaxBackoff)), fmt.Sprintf("TVDB answered %s and asked Garner to wait", resp.Status))
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
	if c.token != "" && c.Now().Sub(c.tokenAt) < tokenMaxAge {
		return c.token, nil
	}
	if err := c.reserve(ctx); err != nil {
		return "", err
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
	respBody, tooLarge, err := readBounded(resp.Body, tvdbMaxResponse)
	if err != nil {
		return "", fmt.Errorf("TVDB login: %w", err)
	}
	if tooLarge {
		return "", fmt.Errorf("TVDB login: response over %d MiB", tvdbMaxResponse>>20)
	}
	var env envelope
	decodeErr := json.Unmarshal(respBody, &env)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return "", c.slowDown(resp)
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", c.pause(c.Now().Add(tvdbRefusedPause), fmt.Sprintf(
			"TVDB refused the API key (%s %s); with Garner's built-in key, set TVDB_API_KEY to a key of your own", resp.Status, env.Message))
	default:
		return "", fmt.Errorf("TVDB login failed (%s): %s", resp.Status, env.Message)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("TVDB login: unreadable response: %w", decodeErr)
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Token == "" {
		return "", errors.New("TVDB login returned no token")
	}
	c.token, c.tokenAt = data.Token, c.Now()
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
		if err := c.reserve(ctx); err != nil {
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
		body, tooLarge, err := readBounded(resp.Body, tvdbMaxResponse)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("TVDB %s: %w", path, err)
		}
		var env envelope
		decodeErr := json.Unmarshal(body, &env)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c.forgetToken()
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			return nil, c.slowDown(resp)
		case tooLarge:
			return nil, fmt.Errorf("TVDB %s: response over %d MiB", path, tvdbMaxResponse>>20)
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("TVDB %s: %s %s", path, resp.Status, env.Message)
		case decodeErr != nil:
			return nil, fmt.Errorf("TVDB %s: unreadable response: %w", path, decodeErr)
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
