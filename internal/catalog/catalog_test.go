package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/store"
)

// TestRefresh loads a show from fake TVDB and TheXEM services, refreshes it
// after TVDB dropped an episode while TheXEM is down, then runs the daily
// sync through a TVDB failure.
func TestRefresh(t *testing.T) {
	ctx := context.Background()
	dropped := false // second round: TVDB lost episode 102, TheXEM fails

	tvdb := http.NewServeMux()
	tvdb.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"token":"tok"}}`)
	})
	reply := func(path, body string) {
		tvdb.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, body)
		})
	}
	broken, loads := false, 0 // for the sync rounds: TVDB fails the show; times it was requested
	tvdb.HandleFunc("GET /series/1/extended", func(w http.ResponseWriter, r *http.Request) {
		loads++
		if broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"id":1,"name":"Ursprung","slug":"show",
		"originalLanguage":"jpn","originalCountry":"jpn","year":"2020","defaultSeasonType":1,"lastUpdated":"2026-01-01",
		"lastAired":"2020-03-25","overview":"Genbun","originalNetwork":{"name":"Example TV"},
		"status":{"name":"Continuing"},"aliases":[{"language":"eng","name":"The Show"}],
		"seasonTypes":[{"id":1,"type":"official","name":"Aired Order"},{"id":2,"type":"alternate","name":"Alternate","alternateName":"Netflix"}]}}`)
	})
	tvdb.HandleFunc("GET /updates", func(w http.ResponseWriter, r *http.Request) {
		changed := ""
		if broken {
			changed = `{"recordId":1}`
		}
		fmt.Fprintf(w, `{"status":"success","data":[%s],"links":{"next":null}}`, changed)
	})
	reply("/series/1/translations/eng", `{"status":"success","data":{"name":"Show","overview":"In English.","aliases":["Show!"]}}`)
	tvdb.HandleFunc("GET /series/1/episodes/official", func(w http.ResponseWriter, r *http.Request) {
		// Two pages, as TVDB splits long shows.
		if r.URL.Query().Get("page") == "0" {
			fmt.Fprint(w, `{"status":"success","data":{"episodes":[
				{"id":901,"seasonNumber":0,"number":1,"name":"Tokubetsu"},
				{"id":101,"seasonNumber":1,"number":1,"name":"Hajimari","aired":"2020-01-01","runtime":24,"absoluteNumber":1,"overview":"Genbun."}]},
				"links":{"next":"page=1"}}`)
			return
		}
		ep102 := `{"id":102,"seasonNumber":1,"number":2,"absoluteNumber":2},`
		if dropped {
			ep102 = ""
		}
		fmt.Fprintf(w, `{"status":"success","data":{"episodes":[%s{"id":201,"seasonNumber":2,"number":1,"absoluteNumber":3,"overview":"Genbun 2."}]},"links":{"next":null}}`, ep102)
	})
	reply("/series/1/episodes/alternate", `{"status":"success","data":{"episodes":[
		{"id":101,"seasonNumber":1,"number":1},{"id":201,"seasonNumber":1,"number":2},{"id":901,"seasonNumber":2,"number":1}]}}`)
	reply("/series/1/episodes/official/eng", `{"status":"success","data":{"episodes":[{"id":101,"name":"The Beginning"}]}}`)
	reply("/series/1/episodes/official/deu", `{"status":"success","data":{"episodes":[{"id":101,"name":"Der Anfang","overview":"Auf Deutsch."},{"id":201,"name":null}]}}`)
	tvdbSrv := httptest.NewServer(tvdb)
	defer tvdbSrv.Close()

	xem := http.NewServeMux()
	xem.HandleFunc("GET /map/all", func(w http.ResponseWriter, r *http.Request) {
		if dropped {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"result":"success","data":[
			{"scene":{"season":1,"episode":1,"absolute":1},"tvdb":{"season":1,"episode":1,"absolute":1}},
			{"scene":{"season":1,"episode":2,"absolute":2},"tvdb":{"season":1,"episode":2,"absolute":2}},
			{"scene":{"season":2,"episode":1,"absolute":3},"tvdb":{"season":2,"episode":1,"absolute":3}},
			{"scene":{"season":2,"episode":2,"absolute":4},"tvdb":{"season":2,"episode":2,"absolute":4}}]}`)
	})
	xem.HandleFunc("GET /map/names", func(w http.ResponseWriter, r *http.Request) {
		// TheXEM sends a title made of digits as a number.
		fmt.Fprint(w, `{"result":"success","data":{"all":{"jp":["Show Original",42]},"1":{"us":["Show II"]},"2":{"us":["Show S2"]}}}`)
	})
	xemSrv := httptest.NewServer(xem)
	defer xemSrv.Close()

	st, err := store.Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client, err := httpx.New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(st, NewTVDB(tvdbSrv.URL, "key", "", client), NewXEM(xemSrv.URL, client), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	season := 2
	c.corrections = corrections{Title: []titleCorrection{{Series: 1, Title: "Show II", Season: &season, Reason: "aired as season 2"}}}

	if err := c.Add(ctx, 1, store.MonitorNew); err != nil {
		t.Fatal(err)
	}
	d, err := st.LoadSeries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s := d.Series; s.NameEng != "Show" || s.NameDeu != "" || s.DefaultOrder != "official" {
		t.Errorf("series = %+v", s)
	}
	// The details page's facts are stored, the overview in English first.
	if s := d.Series; !s.HasDetails || s.Network != "Example TV" || s.LastAired != "2020-03-25" ||
		s.Overview != "In English." || s.OverviewLanguage != "eng" {
		t.Errorf("details: network %q, last aired %q, overview %q (%s), stored %v", s.Network, s.LastAired, s.Overview, s.OverviewLanguage, s.HasDetails)
	}
	if len(d.Orders) != 2 || d.Orders[0].Type != "alternate" || d.Orders[0].AlternateName != "Netflix" {
		t.Errorf("orders = %+v", d.Orders)
	}
	ep := episodeByID(t, d, 101)
	if ep.Name != "Hajimari" || ep.NameEng != "The Beginning" || ep.NameDeu != "Der Anfang" || ep.Aired != "2020-01-01" {
		t.Errorf("episode 101 = %+v", ep)
	}
	// Episode overviews as the show's: English, else German, else the original.
	for id, want := range map[int64]string{101: "Auf Deutsch. (deu)", 201: "Genbun 2. (jpn)"} {
		if ov, lang, err := st.EpisodeOverview(ctx, id); err != nil || ov+" ("+lang+")" != want {
			t.Errorf("episode %d overview = %q (%s), %v; want %s", id, ov, lang, err, want)
		}
	}
	if got := episodeByID(t, d, 901).Numbers; got["official"] != (store.Position{Season: 0, Number: 1}) || got["alternate"] != (store.Position{Season: 2, Number: 1}) {
		t.Errorf("special numbers = %v", got)
	}
	titles := titleSeasons(d)
	want := "42=-/xem Show II=2/garner Show Original=-/xem Show S2=2/xem Show!=-/tvdb The Show=-/tvdb"
	if titles != want {
		t.Errorf("titles = %s\nwant     %s", titles, want)
	}
	if len(d.Scene) != 4 {
		t.Fatalf("scene numbers = %d", len(d.Scene))
	}
	issues := Issues(d)
	if len(issues) != 1 || issues[0].Kind != "missing_episode" || !strings.Contains(issues[0].Message, "TVDB S02E02") {
		t.Errorf("issues = %+v", issues)
	}
	if err := c.Add(ctx, 1, store.MonitorNew); err == nil {
		t.Error("adding the show twice succeeded")
	}

	dropped = true
	if err := c.Refresh(ctx, 1); err != nil {
		t.Fatal(err)
	}
	d, err = st.LoadSeries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range d.Episodes {
		if e.ID == 102 {
			t.Error("episode 102 survived its removal from TVDB")
		}
	}
	if len(d.Scene) != 4 || titleSeasons(d) != want {
		t.Errorf("TheXEM data not kept: %d scene numbers, titles %s", len(d.Scene), titleSeasons(d))
	}
	// The kept mapping for S01E02 now points nowhere.
	if issues := Issues(d); len(issues) != 2 || !strings.Contains(issues[0].Message, "TVDB S01E02") {
		t.Errorf("issues after removal = %+v", issues)
	}

	// Daily sync: TVDB reports the show changed but fails to deliver it. The
	// run is still recorded, and the next run retries only that show.
	yesterday := time.Now().Add(-25 * time.Hour)
	if err := st.SetSyncState(ctx, syncKey, yesterday.UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	broken, loads = true, 0
	if err := c.Sync(ctx); err == nil {
		t.Error("sync with a failing show reported success")
	}
	if last, err := c.lastSync(ctx); err != nil || !last.After(yesterday) {
		t.Errorf("sync not recorded: %v %v", last, err)
	}
	broken = false
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if retry, _ := st.SyncState(ctx, retryKey); loads != 2 || retry != "" {
		t.Errorf("show loaded %d times, retry list %q; want 2 and empty", loads, retry)
	}
}

func episodeByID(t *testing.T, d store.SeriesData, id int64) store.Episode {
	t.Helper()
	for _, e := range d.Episodes {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("episode %d missing", id)
	return store.Episode{}
}

// titleSeasons summarises titles as "title=season/source".
func titleSeasons(d store.SeriesData) string {
	var parts []string
	for _, t := range d.Titles {
		season := "-"
		if t.Season != nil {
			season = fmt.Sprint(*t.Season)
		}
		parts = append(parts, t.Title+"="+season+"/"+t.Source)
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}
