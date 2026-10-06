package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// XEMBaseURL is TheXEM's mapping API.
const XEMBaseURL = "https://thexem.info"

// XEM reads TheXEM's community-maintained scene numbering and titles.
type XEM struct {
	base string
	http *http.Client
}

// NewXEM returns a client.
func NewXEM(base string, client *http.Client) *XEM { return &XEM{base: base, http: client} }

type xemNumber struct {
	Season   int `json:"season"`
	Episode  int `json:"episode"`
	Absolute int `json:"absolute"`
}

// xemMapping maps one TVDB episode (aired order) to scene numbering, the
// numbering releases use.
type xemMapping struct {
	Scene *xemNumber `json:"scene"`
	TVDB  *xemNumber `json:"tvdb"`
}

// xemTitles are alternative titles keyed by scene season ("all" for the
// whole show), then language.
type xemTitles map[string]map[string][]string

func (c *XEM) get(ctx context.Context, path string, q url.Values, out any) error {
	q.Set("origin", "tvdb")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("TheXEM %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TheXEM %s: %s", path, resp.Status)
	}
	var env struct {
		Result  string          `json:"result"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("TheXEM %s: %w", path, err)
	}
	if env.Result != "success" {
		// TheXEM answers "failure" for shows it has no data for.
		if strings.Contains(env.Message, "no show") || strings.Contains(env.Message, "no single connection") {
			return nil
		}
		return fmt.Errorf("TheXEM %s: %s", path, env.Message)
	}
	return json.Unmarshal(env.Data, out)
}

func (c *XEM) mappings(ctx context.Context, tvdbID int64) ([]xemMapping, error) {
	var out []xemMapping
	err := c.get(ctx, "/map/all", url.Values{"id": {strconv.FormatInt(tvdbID, 10)}}, &out)
	return out, err
}

func (c *XEM) titles(ctx context.Context, tvdbID int64) (xemTitles, error) {
	var out xemTitles
	err := c.get(ctx, "/map/names", url.Values{"id": {strconv.FormatInt(tvdbID, 10)}, "seasonNumbers": {"1"}}, &out)
	return out, err
}
