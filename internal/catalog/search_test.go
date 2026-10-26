package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/store"
)

// TestSearchRanking checks that results whose name in any language holds
// every search word come first, in TVDB's order, and that the rest keep
// TVDB's order behind them. A number finds that TVDB ID first, counted once.
func TestSearchRanking(t *testing.T) {
	tvdb := http.NewServeMux()
	tvdb.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"token":"tok"}}`)
	})
	tvdb.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		results := map[string]string{
			// The original name is Japanese; the English one holds the word.
			"wanderer": `{"tvdb_id":"1","name":"Wandeler Peace"},{"tvdb_id":"2","name":"Wandering"},
				{"tvdb_id":"3","name":"放浪者","translations":{"eng":"Wanderer: After the Journey"}},{"tvdb_id":"4","name":"The Wanderer"}`,
			// Spaces in the name, none in the search.
			"belnabel": `{"tvdb_id":"5","name":"Belnaville"},{"tvdb_id":"6","name":"Bel Na Bel"}`,
			// A number is a TVDB ID and a title; TVDB also lists show 7.
			"7": `{"tvdb_id":"8","name":"Seven 7"},{"tvdb_id":"7","name":"Seven"}`,
		}[r.URL.Query().Get("query")]
		total := 52
		if r.URL.Query().Get("query") == "7" {
			total = 2
		}
		fmt.Fprintf(w, `{"status":"success","data":[%s],"links":{"total_items":%d}}`, results, total)
	})
	tvdb.HandleFunc("GET /series/7/extended", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"id":7,"name":"Seven","slug":"seven","status":{"name":"Ended"}}}`)
	})
	srv := httptest.NewServer(tvdb)
	defer srv.Close()
	st, err := store.Open(context.Background(), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client, err := httpx.New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(st, testTVDB(srv.URL, client), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	for q, want := range map[string]string{"wanderer": "[3 4 1 2] of 52", "belnabel": "[6 5] of 52", "7": "[7 8] of 2"} {
		page, err := c.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, r := range page.Results {
			ids = append(ids, r.ID)
		}
		if got := fmt.Sprintf("%v of %d", ids, page.Total); got != want {
			t.Errorf("search %q: %s, want %s", q, got, want)
		}
	}
}

// TestTVDBRef checks which input names one show directly.
func TestTVDBRef(t *testing.T) {
	for in, want := range map[string]string{
		"401234":                            "401234  false",
		"thetvdb.com/series/toro-no-minato": "0 toro-no-minato true",
		"https://thetvdb.com/series/toro-no-minato/seasons/official/1": "0 toro-no-minato true",
		"https://www.thetvdb.com/dereferrer/series/401234":             "401234  true",
		"https://thetvdb.com/?tab=series&id=401234":                    "401234  true",
		"minato":                            "0  false",
		"https://example.com/series/minato": "0  false",
	} {
		id, slug, link := tvdbRef(in)
		if got := fmt.Sprint(id, " ", slug, " ", link); got != want {
			t.Errorf("tvdbRef(%q) = %q, want %q", in, got, want)
		}
	}
}
