package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// setupSABnzbd points SABnzbd at the media folder, the Usenet server from
// devstack.toml and the tv category Garner uses, and removes any other
// server. It returns SABnzbd's API key.
func setupSABnzbd(a accounts) (string, error) {
	u := a.Usenet
	var key string
	if err := waitFor("SABnzbd writing its settings", 2*time.Minute, func() bool {
		key = iniValue(path("config/sabnzbd/sabnzbd.ini"), "misc", "api_key")
		return key != ""
	}); err != nil {
		return "", err
	}
	sab := newClient(sabURL)
	if err := waitFor("SABnzbd answering", 2*time.Minute, func() bool {
		return sab.do(http.MethodGet, "/api?mode=version&output=json", nil, "", nil) == nil
	}); err != nil {
		return "", err
	}
	call := func(params url.Values) error {
		params.Set("apikey", key)
		params.Set("output", "json")
		var res struct {
			Status *bool  `json:"status"`
			Error  string `json:"error"`
		}
		if err := sab.form("/api", params, &res); err != nil {
			return err
		}
		if res.Error != "" || res.Status != nil && !*res.Status {
			return fmt.Errorf("%s %s: %s", params.Get("mode"), params.Get("section"), res.Error)
		}
		return nil
	}
	settings := []url.Values{
		{"mode": {"set_config"}, "section": {"misc"}, "keyword": {"download_dir"}, "value": {"/data/usenet/incomplete"}},
		{"mode": {"set_config"}, "section": {"misc"}, "keyword": {"complete_dir"}, "value": {"/data/usenet/complete"}},
		{"mode": {"set_config"}, "section": {"categories"}, "name": {"tv"}, "dir": {"tv"}},
		{"mode": {"set_config"}, "section": {"servers"}, "name": {u.Host}, "displayname": {u.Host},
			"host": {u.Host}, "port": {strconv.Itoa(u.Port)}, "ssl": {"1"}, "ssl_verify": {"3"},
			"connections": {strconv.Itoa(u.Connections)}, "enable": {"1"},
			"username": {u.Username}, "password": {u.Password}},
	}
	// SABnzbd accepts settings while it is still starting and may then lose
	// them, so apply them until it reports them back.
	err := waitFor("SABnzbd keeping its settings", time.Minute, func() bool {
		for _, s := range settings {
			if call(s) != nil {
				return false
			}
		}
		cfg, err := readSABConfig(sab, key)
		if err != nil {
			return false
		}
		for _, s := range cfg.Servers {
			if s.Name != u.Host && call(url.Values{"mode": {"del_config"}, "section": {"servers"}, "keyword": {s.Name}}) != nil {
				return false
			}
		}
		time.Sleep(2 * time.Second)
		return sabSettled(sab, key, sabServer{Name: u.Host, Port: u.Port, Connections: u.Connections, Enable: 1})
	})
	return key, err
}

type sabConfig struct {
	Misc struct {
		DownloadDir string `json:"download_dir"`
		CompleteDir string `json:"complete_dir"`
	} `json:"misc"`
	Categories []struct{ Name, Dir string } `json:"categories"`
	Servers    []sabServer                  `json:"servers"`
}

type sabServer struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Connections int    `json:"connections"`
	Enable      int    `json:"enable"`
}

func readSABConfig(sab *client, key string) (sabConfig, error) {
	var res struct {
		Config sabConfig `json:"config"`
	}
	err := sab.form("/api", url.Values{"mode": {"get_config"}, "output": {"json"}, "apikey": {key}}, &res)
	return res.Config, err
}

// sabSettled reports whether SABnzbd's settings are the ones setupSABnzbd
// applies.
func sabSettled(sab *client, key string, server sabServer) bool {
	c, err := readSABConfig(sab, key)
	if err != nil {
		return false
	}
	return c.Misc.DownloadDir == "/data/usenet/incomplete" && c.Misc.CompleteDir == "/data/usenet/complete" &&
		slices.ContainsFunc(c.Categories, func(x struct{ Name, Dir string }) bool { return x.Name == "tv" && x.Dir == "tv" }) &&
		slices.Equal(c.Servers, []sabServer{server})
}

// setupProwlarr adds the indexers as Newznab entries with redirect on, so
// SABnzbd fetches NZBs from the indexer itself, and then removes any other
// indexer, so a replacement that Prowlarr rejects leaves the old one in
// place. It returns Prowlarr's API key.
func setupProwlarr(wanted []indexer) (string, error) {
	var key string
	if err := waitFor("Prowlarr writing its settings", 2*time.Minute, func() bool {
		key = xmlValue(path("config/prowlarr/config.xml"), "ApiKey")
		return key != ""
	}); err != nil {
		return "", err
	}
	p := newClient(prowlarrURL)
	p.header.Set("X-Api-Key", key)
	if err := waitFor("Prowlarr answering", 2*time.Minute, func() bool {
		return p.json(http.MethodGet, "/api/v1/system/status", nil, nil) == nil
	}); err != nil {
		return "", err
	}
	var existing []map[string]any
	if err := p.json(http.MethodGet, "/api/v1/indexer", nil, &existing); err != nil {
		return "", err
	}
	var schema []map[string]any
	if err := p.json(http.MethodGet, "/api/v1/indexer/schema", nil, &schema); err != nil {
		return "", err
	}
	i := slices.IndexFunc(schema, func(s map[string]any) bool {
		return s["implementation"] == "Newznab" && s["definitionName"] == "Newznab"
	})
	if i < 0 {
		return "", errors.New("Prowlarr offers no generic Newznab indexer")
	}
	for _, w := range wanted {
		def := schema[i]
		method, target := http.MethodPost, "/api/v1/indexer"
		if j := slices.IndexFunc(existing, func(e map[string]any) bool { return e["name"] == w.Name }); j >= 0 {
			def, method, target = existing[j], http.MethodPut, fmt.Sprintf("/api/v1/indexer/%v", existing[j]["id"])
		}
		def["name"], def["enable"], def["redirect"], def["priority"], def["appProfileId"] = w.Name, true, true, 25, 1
		fields, _ := def["fields"].([]any)
		for _, f := range fields {
			field, _ := f.(map[string]any)
			switch field["name"] {
			case "baseUrl":
				field["value"] = w.URL
			case "apiPath":
				field["value"] = "/api"
			case "apiKey":
				field["value"] = w.APIKey
			}
		}
		// Prowlarr tests the indexer before saving, which checks the key.
		if err := p.json(method, target, def, nil); err != nil {
			return "", fmt.Errorf("indexer %s: %w", w.Name, err)
		}
	}
	for _, e := range existing {
		if !slices.ContainsFunc(wanted, func(w indexer) bool { return w.Name == e["name"] }) {
			if err := p.json(http.MethodDelete, fmt.Sprintf("/api/v1/indexer/%v", e["id"]), nil, nil); err != nil {
				return "", err
			}
		}
	}
	return key, nil
}

const (
	tvdbPlugin     = "a677c0dafac54cde941a7134223f14c8"
	tvdbVersion    = "24.0.0.0"
	pluginManifest = "https://repo.jellyfin.org/files/plugin/manifest.json"
	jellyfinUser   = "garner-dev"
	jellyfinApp    = "garner-dev"
)

// setupJellyfin completes Jellyfin's first-run wizard, creates an API key,
// installs the TVDB plugin and the TV and anime libraries with TVDB first,
// as in production. It returns the API key.
func setupJellyfin() (string, error) {
	jf := newClient(jellyfinURL)
	var info struct{ StartupWizardCompleted bool }
	if err := waitFor("Jellyfin answering", 3*time.Minute, func() bool {
		return jf.json(http.MethodGet, "/System/Info/Public", nil, &info) == nil
	}); err != nil {
		return "", err
	}
	password := readSecret("jellyfin_admin_password")
	if !info.StartupWizardCompleted {
		if password == "" {
			password = randomHex(16)
			if err := writeSecret("jellyfin_admin_password", password); err != nil {
				return "", err
			}
		}
		steps := []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/Startup/Configuration", map[string]string{"ServerName": "garner-dev", "UICulture": "en-US", "MetadataCountryCode": "AT", "PreferredMetadataLanguage": "de"}},
			{http.MethodGet, "/Startup/User", nil},
			{http.MethodPost, "/Startup/User", map[string]string{"Name": jellyfinUser, "Password": password}},
			// The stack is for this machine only; Podman's port forwarding
			// still counts as local.
			{http.MethodPost, "/Startup/RemoteAccess", map[string]bool{"EnableRemoteAccess": false}},
			{http.MethodPost, "/Startup/Complete", nil},
		}
		for _, s := range steps {
			if err := jf.json(s.method, s.path, s.body, nil); err != nil {
				return "", err
			}
		}
	}

	key := readSecret("jellyfin_api_key")
	jf.header.Set("Authorization", `MediaBrowser Token="`+key+`"`)
	if key == "" || jf.json(http.MethodGet, "/System/Info", nil, nil) != nil {
		var err error
		if key, err = newJellyfinKey(password); err != nil {
			return "", err
		}
		jf.header.Set("Authorization", `MediaBrowser Token="`+key+`"`)
	}

	if err := installTVDBPlugin(jf); err != nil {
		return "", err
	}
	if err := ensureLibraries(jf); err != nil {
		return "", err
	}
	return key, nil
}

func newJellyfinKey(password string) (string, error) {
	if password == "" {
		return "", errors.New("the admin password in secrets/jellyfin_admin_password is missing; run `mise run devstack reset`")
	}
	const device = `MediaBrowser Client="garner-devstack", Device="devstack", DeviceId="garner-devstack", Version="1.0"`
	jf := newClient(jellyfinURL)
	jf.header.Set("Authorization", device)
	var auth struct{ AccessToken string }
	if err := jf.json(http.MethodPost, "/Users/AuthenticateByName", map[string]string{"Username": jellyfinUser, "Pw": password}, &auth); err != nil {
		return "", fmt.Errorf("signing in as %s: %w", jellyfinUser, err)
	}
	jf.header.Set("Authorization", device+`, Token="`+auth.AccessToken+`"`)
	var keys struct {
		Items []struct{ AccessToken, AppName, DateCreated string }
	}
	// Revoke earlier keys of the stack, which no longer work for it anyway.
	if err := jf.json(http.MethodGet, "/Auth/Keys", nil, &keys); err != nil {
		return "", err
	}
	for _, k := range keys.Items {
		if k.AppName == jellyfinApp {
			if err := jf.json(http.MethodDelete, "/Auth/Keys/"+url.PathEscape(k.AccessToken), nil, nil); err != nil {
				return "", err
			}
		}
	}
	if err := jf.json(http.MethodPost, "/Auth/Keys?app="+jellyfinApp, nil, nil); err != nil {
		return "", err
	}
	if err := jf.json(http.MethodGet, "/Auth/Keys", nil, &keys); err != nil {
		return "", err
	}
	newest := ""
	latest := ""
	for _, k := range keys.Items {
		if k.AppName == jellyfinApp && k.DateCreated >= latest {
			newest, latest = k.AccessToken, k.DateCreated
		}
	}
	if newest == "" {
		return "", errors.New("Jellyfin did not list the API key it created")
	}
	return newest, nil
}

func installTVDBPlugin(jf *client) error {
	type plugin struct{ Id, Name, Status string }
	plugins := func() []plugin {
		var list []plugin
		jf.json(http.MethodGet, "/Plugins", nil, &list)
		return list
	}
	has := func() bool { return slices.ContainsFunc(plugins(), func(p plugin) bool { return p.Id == tvdbPlugin }) }
	if !has() {
		step("installing the TheTVDB plugin")
		q := url.Values{"assemblyGuid": {tvdbPlugin}, "version": {tvdbVersion}, "repositoryUrl": {pluginManifest}}
		if err := jf.json(http.MethodPost, "/Packages/Installed/TheTVDB?"+q.Encode(), nil, nil); err != nil {
			return err
		}
		if err := waitFor("the TheTVDB plugin download", 3*time.Minute, has); err != nil {
			return err
		}
	}
	var info struct{ HasPendingRestart bool }
	if err := jf.json(http.MethodGet, "/System/Info", nil, &info); err != nil || !info.HasPendingRestart {
		return err
	}
	step("restarting Jellyfin to load the plugin")
	if err := jf.json(http.MethodPost, "/System/Restart", nil, nil); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	return waitFor("Jellyfin restarting", 3*time.Minute, func() bool {
		var after struct{ HasPendingRestart bool }
		return jf.json(http.MethodGet, "/System/Info", nil, &after) == nil && !after.HasPendingRestart &&
			slices.ContainsFunc(plugins(), func(p plugin) bool { return p.Id == tvdbPlugin && p.Status == "Active" })
	})
}

// ensureLibraries creates the TV and anime libraries and sets TVDB first
// for shows, seasons and episodes. TMDb and OMDb stay as show-level
// fallbacks only: for seasons and episodes they number differently.
func ensureLibraries(jf *client) error {
	libraries := []struct{ name, folder string }{{"TV Shows", "/data/library/tv"}, {"Anime", "/data/library/anime"}}
	type folder struct {
		Name           string
		ItemId         string
		LibraryOptions map[string]any
	}
	var folders []folder
	if err := jf.json(http.MethodGet, "/Library/VirtualFolders", nil, &folders); err != nil {
		return err
	}
	for _, lib := range libraries {
		if slices.ContainsFunc(folders, func(f folder) bool { return f.Name == lib.name }) {
			continue
		}
		q := url.Values{"name": {lib.name}, "collectionType": {"tvshows"}, "paths": {lib.folder}, "refreshLibrary": {"false"}}
		opts := map[string]any{"LibraryOptions": map[string]any{"PreferredMetadataLanguage": "de", "MetadataCountryCode": "AT", "EnableRealtimeMonitor": true}}
		if err := jf.json(http.MethodPost, "/Library/VirtualFolders?"+q.Encode(), opts, nil); err != nil {
			return fmt.Errorf("library %s: %w", lib.name, err)
		}
	}
	if err := jf.json(http.MethodGet, "/Library/VirtualFolders", nil, &folders); err != nil {
		return err
	}
	const tv, tmdb, omdb = "TheTVDB", "TheMovieDb", "The Open Movie Database"
	want := map[string]map[string][]string{
		"Series": {"MetadataFetchers": {tv, tmdb, omdb}, "MetadataFetcherOrder": {tv, tmdb, omdb, "Missing Episode Fetcher"},
			"ImageFetchers": {tv, tmdb}, "ImageFetcherOrder": {tv, tmdb}},
		"Season": {"MetadataFetchers": {tv}, "MetadataFetcherOrder": {tv, tmdb},
			"ImageFetchers": {tv}, "ImageFetcherOrder": {tv, tmdb}},
		"Episode": {"MetadataFetchers": {tv}, "MetadataFetcherOrder": {tv, tmdb, omdb},
			"ImageFetchers":     {tv, "Embedded Image Extractor", "Screen Grabber"},
			"ImageFetcherOrder": {tv, "Embedded Image Extractor", "Screen Grabber", tmdb, omdb}},
	}
	for _, f := range folders {
		if !slices.ContainsFunc(libraries, func(l struct{ name, folder string }) bool { return l.name == f.Name }) {
			continue
		}
		opts := f.LibraryOptions
		existing, _ := opts["TypeOptions"].([]any)
		var types []any
		for _, t := range existing {
			if m, ok := t.(map[string]any); ok && want[fmt.Sprint(m["Type"])] == nil {
				types = append(types, m)
			}
		}
		for _, name := range []string{"Series", "Season", "Episode"} {
			t := map[string]any{"Type": name, "ImageOptions": []any{}}
			for k, v := range want[name] {
				t[k] = v
			}
			types = append(types, t)
		}
		opts["TypeOptions"] = types
		if err := jf.json(http.MethodPost, "/Library/VirtualFolders/LibraryOptions", map[string]any{"Id": f.ItemId, "LibraryOptions": opts}, nil); err != nil {
			return fmt.Errorf("library %s options: %w", f.Name, err)
		}
	}
	return nil
}

// client talks to one service.
type client struct {
	base   string
	header http.Header
}

func newClient(base string) *client { return &client{base: base, header: http.Header{}} }

var httpClient = &http.Client{Timeout: time.Minute}

func (c *client) json(method, p string, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, contentType = strings.NewReader(string(b)), "application/json"
	}
	return c.do(method, p, body, contentType, out)
}

func (c *client) form(p string, v url.Values, out any) error {
	return c.do(http.MethodPost, p, strings.NewReader(v.Encode()), "application/x-www-form-urlencoded", out)
}

// do sends a request. Errors never quote response bodies, which may echo
// submitted secrets; they only quote the error messages a service returns.
func (c *client) do(method, p string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.base+p, body)
	if err != nil {
		return err
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s%s", method, strings.SplitN(p, "?", 2)[0], resp.Status, messages(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// messages extracts error messages from an error response, without
// fields such as Prowlarr's attemptedValue that may hold a submitted key.
func messages(data []byte) string {
	var list []struct{ ErrorMessage string }
	if json.Unmarshal(data, &list) == nil {
		var out []string
		for _, e := range list {
			if e.ErrorMessage != "" {
				out = append(out, e.ErrorMessage)
			}
		}
		if len(out) > 0 {
			return ": " + strings.Join(out, "; ")
		}
	}
	var obj struct{ Message, Error string }
	if json.Unmarshal(data, &obj) == nil && (obj.Message != "" || obj.Error != "") {
		return ": " + obj.Message + obj.Error
	}
	return ""
}

// iniValue reads a key from a section of SABnzbd's settings file.
func iniValue(file, section, key string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	current := ""
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "[") {
			current = strings.Trim(line, "[]")
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && current == section && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// xmlValue reads an element from Prowlarr's flat settings file.
func xmlValue(file, element string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`<` + element + `>([^<]*)</` + element + `>`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
