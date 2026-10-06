package prowlarr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func item(title string, langs ...string) string {
	var attrs strings.Builder
	for _, l := range langs {
		fmt.Fprintf(&attrs, `<newznab:attr name="language" value="%s"/>`, l)
	}
	return fmt.Sprintf(`<item><title>%s</title><link>http://prowlarr/1/download?apikey=SECRET&amp;link=X</link><size>1000</size>
		<pubDate>Thu, 01 Oct 2026 19:28:29 +0000</pubDate>%s<newznab:attr name="tvdbid" value="42"/><newznab:attr name="grabs" value="7"/></item>`, title, attrs.String())
}

func feedOf(items ...string) string {
	return `<?xml version="1.0"?><rss xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/"><channel>` +
		strings.Join(items, "") + `</channel></rss>`
}

func TestSearch(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" || r.URL.Query().Has("apikey") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/indexer":
			fmt.Fprint(w, `[{"id":1,"name":"One","enable":true,"protocol":"usenet"},{"id":2,"name":"Two","enable":true,"protocol":"usenet"},
				{"id":3,"name":"Off","enable":false,"protocol":"usenet"},{"id":4,"name":"Torrents","enable":true,"protocol":"torrent"}]`)
		case "/1/api":
			queries = append(queries, r.URL.RawQuery)
			// A full first page makes the client ask for the next one.
			n := 100
			if r.URL.Query().Get("offset") != "0" {
				n = 2
			}
			items := make([]string, n)
			for i := range items {
				items[i] = item(fmt.Sprintf("Show.S01E01.%d.German.DL.1080p.WEB-GRP", i), "German", "English")
			}
			fmt.Fprint(w, feedOf(items...))
		case "/2/api":
			fmt.Fprint(w, `<?xml version="1.0"?><error code="100" description="Incorrect user credentials"/>`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "key", srv.Client())
	ctx := context.Background()

	indexers, err := c.Indexers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexers) != 2 || indexers[0].Name != "One" || indexers[1].Name != "Two" {
		t.Fatalf("indexers = %+v", indexers)
	}

	start := time.Now()
	results, err := c.Search(ctx, 1, Query{TVDBID: 42, Season: 1, Episode: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 102 || len(queries) != 2 || !strings.Contains(queries[0], "tvdbid=42") || !strings.Contains(queries[0], "ep=1") {
		t.Fatalf("%d results from queries %q", len(results), queries)
	}
	if elapsed := time.Since(start); elapsed < requestGap {
		t.Errorf("two requests took %s; want at least %s apart", elapsed, requestGap)
	}
	r := results[0]
	if r.Size != 1000 || r.Grabs != 7 || r.TVDBID != 42 || !slices.Equal(r.Languages, []string{"German", "English"}) || r.Published.IsZero() {
		t.Errorf("result = %+v", r)
	}

	_, err = c.Search(ctx, 2, Query{TVDBID: 42, Season: 1})
	if err == nil || !strings.Contains(err.Error(), "Incorrect user credentials") {
		t.Errorf("error = %v", err)
	}
}
