package client

import (
	"context"
	"net/http"
	"strconv"

	"github.com/trucke/garner/internal/api"
)

func profilePath(id int64) string { return "/api/v1/profiles/" + strconv.FormatInt(id, 10) }

// Profiles lists the stored profiles by name.
func (c *Client) Profiles(ctx context.Context) ([]api.Profile, error) {
	var out []api.Profile
	return out, c.do(ctx, http.MethodGet, "/api/v1/profiles", nil, &out)
}

// Profile returns one stored profile.
func (c *Client) Profile(ctx context.Context, id int64) (api.Profile, error) {
	var out api.Profile
	return out, c.do(ctx, http.MethodGet, profilePath(id), nil, &out)
}

// CreateProfile saves a profile's text as a new profile. A profile with
// problems fails with a *ServerError that lists them.
func (c *Client) CreateProfile(ctx context.Context, text string) (api.Profile, error) {
	var out api.Profile
	return out, c.do(ctx, http.MethodPost, "/api/v1/profiles", map[string]string{"text": text}, &out)
}

// ReplaceProfile saves a profile's text in place of a stored profile; the
// shows that use it follow the new settings.
func (c *Client) ReplaceProfile(ctx context.Context, id int64, text string) (api.Profile, error) {
	var out api.Profile
	return out, c.do(ctx, http.MethodPut, profilePath(id), map[string]string{"text": text}, &out)
}

// DeleteProfile deletes a profile no show uses and that is no default.
func (c *Client) DeleteProfile(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, profilePath(id), nil, nil)
}

// SetDefaultProfile makes a profile the one new shows of the types get,
// api.TypeTV, api.TypeAnime or both.
func (c *Client) SetDefaultProfile(ctx context.Context, id int64, types ...string) (api.Profile, error) {
	var out api.Profile
	return out, c.do(ctx, http.MethodPut, profilePath(id)+"/default", map[string][]string{"types": types}, &out)
}
