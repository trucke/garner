package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/profile"
	"github.com/trucke/garner/internal/store"
)

const germanText = `# shared by a friend
garner_profile = 1
name   = "German"
audio = ["de"]
resolution = ["1080p", "720p"]
`

// profileServer is a server on a new database with one TV show, 42, and
// its client API.
func profileServer(t *testing.T) (*Server, *store.Store, func(method, path, body string) (int, string)) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSeries(ctx, store.SeriesData{Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct",
		OriginalLanguage: "eng", OriginalCountry: "usa", Status: "Ended", DefaultOrder: "official", TVDBUpdated: "2026-01-01", Type: store.TypeTV},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &config.Config{}, store: st, log: log, events: newBroker(), tlsMode: TLSOff}
	if err := s.loadProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	token, err := st.CreateToken(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return s, st, call
}

func textBody(text string) string {
	b, _ := json.Marshal(map[string]string{"text": text})
	return string(b)
}

// TestProfileAPI manages profiles as the terminal interface does: every
// text passes the profile check and is stored in its canonical form, a
// change takes effect at once, and a profile that shows use or that is a
// default stays.
func TestProfileAPI(t *testing.T) {
	_, _, call := profileServer(t)
	var list []api.Profile
	code, body := call(http.MethodGet, "/api/v1/profiles", "")
	if json.Unmarshal([]byte(body), &list); code != http.StatusOK || len(list) != 1 || list[0].Name != "Original language" ||
		!list[0].DefaultTV || !list[0].DefaultAnime || !slices.Equal(list[0].Shows, []string{"Harbor Precinct"}) {
		t.Fatalf("a new installation's profiles: %d %s", code, body)
	}
	original := list[0].ID

	var german api.Profile
	code, body = call(http.MethodPost, "/api/v1/profiles", textBody(germanText))
	if json.Unmarshal([]byte(body), &german); code != http.StatusCreated || german.Name != "German" || strings.Contains(german.Text, "friend") {
		t.Fatalf("create: %d %s", code, body)
	}
	if p, _, err := profile.Parse(german.Text); err != nil || profile.Encode(p) != german.Text {
		t.Errorf("stored text is not canonical: %v\n%s", err, german.Text)
	}
	var problems struct{ Problems []string }
	code, body = call(http.MethodPost, "/api/v1/profiles", textBody("garner_profile = 1\naudio = [\"German\"]\n"))
	if json.Unmarshal([]byte(body), &problems); code != http.StatusBadRequest || len(problems.Problems) != 3 {
		t.Errorf("a profile with problems: %d %s", code, body)
	}
	if code, body := call(http.MethodPost, "/api/v1/profiles", textBody(germanText)); code != http.StatusConflict {
		t.Errorf("a second German: %d %s", code, body)
	}

	// The show takes German; replacing German changes its rules at once.
	rules := func() string {
		t.Helper()
		var d api.SeriesDetail
		code, body := call(http.MethodPatch, "/api/v1/series/42", `{"monitor":"all"}`)
		if json.Unmarshal([]byte(body), &d); code != http.StatusOK {
			t.Fatalf("series: %d %s", code, body)
		}
		return d.Profile + ": " + d.Rules
	}
	if code, body := call(http.MethodPatch, "/api/v1/series/42", fmt.Sprintf(`{"profile_id":%d}`, german.ID)); code != http.StatusOK {
		t.Fatalf("give the show German: %d %s", code, body)
	}
	if got := rules(); got != "German: German 1080p → German 720p · dual audio preferred" {
		t.Errorf("rules: %q", got)
	}
	german2 := strings.Replace(germanText, `["de"]`, `["de", "en"]`, 1)
	if code, body := call(http.MethodPut, fmt.Sprintf("/api/v1/profiles/%d", german.ID), textBody(german2)); code != http.StatusOK {
		t.Fatalf("replace: %d %s", code, body)
	}
	if got := rules(); got != "German: German 1080p → German 720p → English 1080p → English 720p · dual audio preferred" {
		t.Errorf("rules after the change: %q", got)
	}
	renamed := strings.Replace(germanText, `"German"`, `"Original language"`, 1)
	if code, _ := call(http.MethodPut, fmt.Sprintf("/api/v1/profiles/%d", german.ID), textBody(renamed)); code != http.StatusConflict {
		t.Errorf("rename to a taken name: %d", code)
	}

	// German becomes the default for both types; it stays while the show
	// uses it, and Original language, now unused, goes.
	if code, body := call(http.MethodPut, fmt.Sprintf("/api/v1/profiles/%d/default", german.ID), `{"types":["tv","anime"]}`); code != http.StatusOK ||
		!strings.Contains(body, `"default_tv":true,"default_anime":true`) {
		t.Errorf("default: %d %s", code, body)
	}
	if code, body := call(http.MethodPut, fmt.Sprintf("/api/v1/profiles/%d/default", german.ID), `{"types":["movies"]}`); code != http.StatusBadRequest {
		t.Errorf("an unknown type: %d %s", code, body)
	}
	if code, body := call(http.MethodDelete, fmt.Sprintf("/api/v1/profiles/%d", german.ID), ""); code != http.StatusConflict ||
		!strings.Contains(body, "Harbor Precinct") || !strings.Contains(body, "new TV shows") {
		t.Errorf("delete a profile in use: %d %s", code, body)
	}
	if code, body := call(http.MethodDelete, fmt.Sprintf("/api/v1/profiles/%d", original), ""); code != http.StatusNoContent {
		t.Errorf("delete an unused profile: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, fmt.Sprintf("/api/v1/profiles/%d", original), ""},
		{http.MethodPut, "/api/v1/profiles/999", textBody(germanText)},
		{http.MethodDelete, "/api/v1/profiles/999", ""},
	} {
		if code, _ := call(c.method, c.path, c.body); code != http.StatusNotFound {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
}

// TestImportProfile imports profiles from the command line: a profile of
// the same name stays unless the import replaces it, and problems come
// back one by one.
func TestImportProfile(t *testing.T) {
	s, _, _ := profileServer(t)
	srv := httptest.NewServer(s.AdminRoutes())
	t.Cleanup(srv.Close)
	admin := &Admin{client: &http.Client{Transport: rewrite{srv.Listener.Addr().String()}}}
	ctx := context.Background()

	if got, err := admin.ImportProfile(ctx, germanText, false); err != nil || got.Replaced || got.Name != "German" {
		t.Fatalf("import: %+v, %v", got, err)
	}
	if _, err := admin.ImportProfile(ctx, germanText, false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("import again: %v", err)
	}
	if got, err := admin.ImportProfile(ctx, germanText, true); err != nil || !got.Replaced {
		t.Errorf("replace: %+v, %v", got, err)
	}
	_, err := admin.ImportProfile(ctx, "garner_profile = 1\nname = \"x\"\naudio = [\"German\"]\nresolution = [\"4k\"]\n", false)
	var pe *profile.Error
	if !errors.As(err, &pe) || len(pe.Problems) != 2 {
		t.Errorf("a profile with problems: %v", err)
	}
	warned := strings.Replace(germanText, "audio =", "priority = \"resolution\"\naudio =", 1)
	if got, err := admin.ImportProfile(ctx, warned, true); err != nil || len(got.Warnings) != 1 {
		t.Errorf("a warning: %+v, %v", got, err)
	}
}
