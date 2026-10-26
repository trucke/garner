package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
	"github.com/trucke/garner/internal/testprofiles"
)

// testTVDB is a client whose pacing does not wait, for tests about other
// things than the limits.
func testTVDB(base string, client *http.Client) *TVDB {
	c := NewTVDB(base, "key", "", client)
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// fakeTVDB answers as TheTVDB and records when, on the client's clock,
// each request arrived. login and series answer with the status they hold;
// retryAfter goes with a 429. The test, the client and the handler share
// it, so every field is read and written under mu.
type fakeTVDB struct {
	mu         sync.Mutex
	now        time.Time
	at         []time.Time
	logins     int
	login      int
	series     int
	retryAfter string
}

func newFakeTVDB(login, series int) *fakeTVDB {
	return &fakeTVDB{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), login: login, series: series}
}

func (f *fakeTVDB) serve(t *testing.T) *TVDB {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.at, f.logins = append(f.at, f.now), f.logins+1
		status := f.login
		f.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"status":"success","data":{"token":"tok"}}`))
	})
	mux.HandleFunc("GET /series/1/extended", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.at = append(f.at, f.now)
		status, retryAfter := f.series, f.retryAfter
		f.mu.Unlock()
		if status == http.StatusTooManyRequests && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"status":"success","data":{"id":1,"name":"Harbor Precinct"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := httpx.New("test", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := NewTVDB(srv.URL, "key", "", client)
	c.Now = f.clock
	c.Sleep = func(_ context.Context, d time.Duration) error { f.set(f.clock().Add(d)); return nil }
	return c
}

func (f *fakeTVDB) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTVDB) set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// answer sets the status that login and series answer with.
func (f *fakeTVDB) answer(login, series int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.login, f.series = login, series
}

// requests returns when each request arrived, and how many logins came.
func (f *fakeTVDB) requests() ([]time.Time, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.at), f.logins
}

func getSeries(c *TVDB) error {
	var out tvdbSeries
	_, err := c.get(context.Background(), "/series/1/extended", &out)
	return err
}

// TestTVDBPace sends a burst of requests: they leave at most five per
// second, so one installation never floods TheTVDB.
func TestTVDBPace(t *testing.T) {
	f := newFakeTVDB(http.StatusOK, http.StatusOK)
	c := f.serve(t)
	for range 10 {
		if err := getSeries(c); err != nil {
			t.Fatal(err)
		}
	}
	at, _ := f.requests()
	if len(at) != 11 {
		t.Fatalf("%d requests, want a login and 10 gets", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < tvdbInterval {
			t.Errorf("request %d came %s after the one before, less than %s", i, gap, tvdbInterval)
		}
	}
}

// TestTVDBBudget spends the day's budget: the next request fails without
// reaching TheTVDB, says why, and requests flow again after the window.
func TestTVDBBudget(t *testing.T) {
	f := newFakeTVDB(http.StatusOK, http.StatusOK)
	c := f.serve(t)
	if err := getSeries(c); err != nil {
		t.Fatal(err)
	}
	c.lim.Lock()
	start := c.windowStart
	c.used = tvdbBudget // as if the rest of the day's requests went out
	c.lim.Unlock()

	sent, _ := f.requests()
	err := getSeries(c)
	var paused *PausedError
	if !errors.As(err, &paused) || !paused.Until.Equal(start.Add(tvdbWindow)) || !strings.Contains(paused.Reason, "please report it") {
		t.Fatalf("over budget: %v", err)
	}
	if until, _ := c.Paused(); !until.Equal(paused.Until) {
		t.Errorf("Paused reports %v, want %v", until, paused.Until)
	}
	f.set(start.Add(tvdbWindow - time.Minute))
	if err := getSeries(c); !errors.As(err, &paused) {
		t.Fatalf("a minute before the window ends: %v", err)
	}
	if at, _ := f.requests(); len(at) != len(sent) {
		t.Fatalf("%d requests reached TheTVDB over budget", len(at)-len(sent))
	}
	f.set(start.Add(tvdbWindow))
	if err := getSeries(c); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// TestTVDBSlowDown answers 429: Garner holds every request back for as long
// as TheTVDB asks, without retrying meanwhile.
func TestTVDBSlowDown(t *testing.T) {
	f := newFakeTVDB(http.StatusOK, http.StatusTooManyRequests)
	f.retryAfter = "120"
	c := f.serve(t)
	var paused *PausedError
	if err := getSeries(c); !errors.As(err, &paused) {
		t.Fatalf("429: %v", err)
	}
	asked := f.clock()
	sent, _ := f.requests()
	f.answer(http.StatusOK, http.StatusOK)
	f.set(asked.Add(119 * time.Second))
	if err := getSeries(c); !errors.As(err, &paused) {
		t.Fatalf("before Retry-After ran out: %v", err)
	}
	if at, _ := f.requests(); len(at) != len(sent) {
		t.Fatalf("%d requests reached TheTVDB before Retry-After ran out", len(at)-len(sent))
	}
	f.set(asked.Add(120 * time.Second))
	if err := getSeries(c); err != nil {
		t.Fatalf("after Retry-After: %v", err)
	}
}

// TestTVDBRetryAfter reads how long TheTVDB asks Garner to wait.
func TestTVDBRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	c := NewTVDB("", "key", "", nil)
	c.Now = func() time.Time { return now }
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"120", 2 * time.Minute},
		{"", tvdbBackoff},
		{"soon", tvdbBackoff},
		{"0", tvdbBackoff},
		{now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{now.Add(-time.Hour).Format(http.TimeFormat), tvdbBackoff},
		{"9999999", tvdbMaxBackoff},
	} {
		c.pausedUntil = time.Time{}
		resp := &http.Response{Status: "429 Too Many Requests", Header: http.Header{}}
		if tc.header != "" {
			resp.Header.Set("Retry-After", tc.header)
		}
		if got := c.slowDown(resp).Until.Sub(now); got != tc.want {
			t.Errorf("Retry-After %q: waits %s, want %s", tc.header, got, tc.want)
		}
	}
}

// TestTVDBRefusedKey refuses the key: Garner stops logging in for an hour
// and names TVDB_API_KEY as the way out.
func TestTVDBRefusedKey(t *testing.T) {
	f := newFakeTVDB(http.StatusUnauthorized, http.StatusOK)
	c := f.serve(t)
	var paused *PausedError
	if err := getSeries(c); !errors.As(err, &paused) || !strings.Contains(paused.Reason, "TVDB_API_KEY") {
		t.Fatalf("refused key: %v", err)
	}
	refused := f.clock()
	for _, after := range []time.Duration{time.Minute, tvdbRefusedPause - time.Second} {
		f.set(refused.Add(after))
		if err := getSeries(c); !errors.As(err, &paused) {
			t.Fatalf("%s after the refusal: %v", after, err)
		}
	}
	if _, logins := f.requests(); logins != 1 {
		t.Fatalf("%d logins within the hour, want 1", logins)
	}
	f.answer(http.StatusOK, http.StatusOK)
	f.set(refused.Add(tvdbRefusedPause))
	if err := getSeries(c); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

// defaultGerman is the default profile of an installation from before
// profiles moved into the database: German for TV, German and English for
// anime.
func defaultGerman(typ string, original release.Language) selection.Profile {
	return testprofiles.ByType(store.Series{Type: typ, OriginalLanguage: string(original)})
}
