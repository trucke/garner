package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/store"
)

// fakeSAB keeps a queue and a history like SABnzbd 5.1 and answers its API.
type fakeSAB struct {
	mu      sync.Mutex
	added   []string // job names submitted
	queue   []map[string]any
	history []map[string]any
}

func (f *fakeSAB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	matches := func(job map[string]any, nameKey string) bool {
		if ids := r.PostFormValue("nzo_ids"); ids != "" {
			return slices.Contains(strings.Split(ids, ","), job["nzo_id"].(string))
		}
		return strings.Contains(job[nameKey].(string), r.PostFormValue("search"))
	}
	var out any
	switch r.PostFormValue("mode") {
	case "addurl":
		id := fmt.Sprintf("job-%d", len(f.added)+1)
		f.added = append(f.added, r.PostFormValue("nzbname"))
		f.queue = append(f.queue, map[string]any{"nzo_id": id, "filename": r.PostFormValue("nzbname"), "cat": r.PostFormValue("cat"),
			"status": "Grabbing", "percentage": "0", "timeleft": "0:00:00", "time_added": time.Now().Unix()})
		out = map[string]any{"status": true, "nzo_ids": []string{id}}
	case "queue":
		if r.PostFormValue("name") == "delete" {
			f.queue = slices.DeleteFunc(f.queue, func(j map[string]any) bool { return j["nzo_id"] == r.PostFormValue("value") })
			out = map[string]any{"status": true}
			break
		}
		slots := []map[string]any{}
		for _, j := range f.queue {
			if matches(j, "filename") {
				slots = append(slots, j)
			}
		}
		out = map[string]any{"queue": map[string]any{"slots": slots}}
	case "history":
		slots := []map[string]any{}
		for _, j := range f.history {
			if matches(j, "name") {
				slots = append(slots, j)
			}
		}
		out = map[string]any{"history": map[string]any{"slots": slots}}
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeSAB) progress(id, status string, percent int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.queue {
		if j["nzo_id"] == id {
			j["status"], j["percentage"] = status, fmt.Sprint(percent)
		}
	}
}

// finish moves a job from the queue to the history.
func (f *fakeSAB) finish(id, status, failure, storage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.queue, func(j map[string]any) bool { return j["nzo_id"] == id })
	j := f.queue[i]
	f.queue = slices.Delete(f.queue, i, i+1)
	f.history = append(f.history, map[string]any{"nzo_id": id, "name": j["filename"], "category": j["cat"], "status": status,
		"fail_message": failure, "storage": storage, "time_added": j["time_added"],
		"stage_log": []map[string]any{{"name": "Source", "actions": []string{"http://prowlarr/1/download?apikey=SECRET&file=x"}}}})
}

func (f *fakeSAB) submitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added)
}

const show = "Harbor.Precinct.S01E01."

// setup returns a store with a made-up show whose episode 101 has been
// searched, and a service talking to a fake SABnzbd.
func setup(t *testing.T) (*store.Store, *Service, *fakeSAB) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSeries(ctx, store.SeriesData{
		Series: store.Series{ID: 42, Slug: "harbor-precinct", Name: "Harbor Precinct", OriginalLanguage: "eng",
			OriginalCountry: "usa", Status: "Continuing", DefaultOrder: "official", TVDBUpdated: "2026-01-01"},
		Episodes: []store.Episode{{ID: 101}, {ID: 102}},
	}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	var candidates []store.Candidate
	for _, c := range []struct{ name, verdict string }{
		{"German.DL.1080p.BluRay-A", "accept"}, {"German.DL.1080p.WEB-B", "accept"},
		{"German.DL.WEB-C", "fallback"}, {"1080p.WEB-D", "reject"},
	} {
		candidates = append(candidates, store.Candidate{Indexer: "One", Title: show + c.name, Verdict: c.verdict,
			Link: "http://prowlarr/1/download?apikey=SECRET&file=" + c.name, Reasons: []string{"why " + c.name}, Decisive: 1})
	}
	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: time.Now().Add(-time.Hour), Outcome: "found",
		Summary: "Found 2 matching releases", Candidates: candidates}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSAB{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return st, New(st, sabnzbd.New(srv.URL, "key", srv.Client()), "tv", log), fake
}

// TestDownload follows releases through SABnzbd: progress, a failure that
// another release may avoid, one it cannot, and a completed manual choice.
func TestDownload(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	var published []store.Download
	svc.OnChange = func(d store.Download) { published = append(published, d) }

	d, err := svc.Best(ctx, 101)
	if err != nil || d.State != store.Queued || d.JobID != "job-1" || d.Title != show+"German.DL.1080p.BluRay-A" {
		t.Fatalf("Best = %+v, %v", d, err)
	}
	if _, err := svc.Best(ctx, 101); !errors.Is(err, store.ErrDownloadRunning) {
		t.Errorf("second download while one runs: %v", err)
	}
	fake.progress("job-1", "Downloading", 40)
	svc.poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Downloading || d.Progress != 40 {
		t.Errorf("after progress: %+v", d)
	}

	// Missing articles: another release may succeed, so B starts.
	fake.finish("job-1", "Failed", "Aborted, cannot be completed - https://sabnzbd.org/not-complete", "")
	svc.poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Failed || !d.Retryable {
		t.Errorf("after missing articles: %+v", d)
	}
	if got := fake.submitted(); len(got) != 2 || got[1] != show+"German.DL.1080p.WEB-B" {
		t.Fatalf("submitted %q; want the next release", got)
	}

	// A full disk would stop any release, so nothing new starts.
	fake.finish("job-2", "Failed", "Unpacking failed, write error or disk is full?", "")
	svc.poll(ctx)
	if got := fake.submitted(); len(got) != 2 {
		t.Errorf("submitted %q after a full disk; want no new release", got)
	}

	// Any candidate can be chosen by hand, even a rejected one.
	links, _ := st.CandidateLinks(ctx, 101)
	d, err = svc.Candidate(ctx, links[3].ID)
	if err != nil || !d.Manual {
		t.Fatalf("Candidate = %+v, %v", d, err)
	}
	fake.finish("job-3", "Completed", "", "/data/usenet/complete/tv/x")
	svc.poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Completed || d.Storage != "/data/usenet/complete/tv/x" {
		t.Errorf("after completion: %+v", d)
	}
	all, _ := json.Marshal(published)
	if strings.Contains(string(all), "SECRET") {
		t.Error("a published download holds the download link's API key")
	}
}

// TestReplaceAccepted checks that a failed accepted release, as the monitor
// starts them, is replaced by accepted ones only, never by a last resort.
func TestReplaceAccepted(t *testing.T) {
	ctx := context.Background()
	_, svc, fake := setup(t)
	if _, err := svc.Accepted(ctx, 101); err != nil {
		t.Fatal(err)
	}
	fake.finish("job-1", "Failed", "Aborted, cannot be completed", "")
	svc.poll(ctx)
	fake.finish("job-2", "Failed", "Aborted, cannot be completed", "")
	svc.poll(ctx)
	if got := fake.submitted(); len(got) != 2 || got[1] != show+"German.DL.1080p.WEB-B" {
		t.Fatalf("submitted %q; want A, then B, and no last resort", got)
	}
}

// TestRestart covers a crash between recording a download and SABnzbd's
// answer: the job is found by name instead of submitted again, and a job
// SABnzbd never got ends as failed.
func TestRestart(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	start := time.Now()
	svc.now = func() time.Time { return start }
	links, _ := st.CandidateLinks(ctx, 101)
	got, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: links[0].Title, Link: links[0].Link, Verdict: "accept"}, start)
	if err != nil {
		t.Fatal(err)
	}
	// SABnzbd took the job before the crash.
	fake.queue = append(fake.queue, map[string]any{"nzo_id": "job-9", "filename": links[0].Title, "cat": "tv",
		"status": "Downloading", "percentage": "10", "timeleft": "0:01:00", "time_added": start.Unix()})
	lost, err := st.StartDownload(ctx, store.Download{EpisodeID: 102, Title: "Harbor.Precinct.S01E02.German.DL.1080p.WEB-X",
		Link: "http://prowlarr/1/download?apikey=SECRET", Verdict: "accept"}, start)
	if err != nil {
		t.Fatal(err)
	}

	svc.now = func() time.Time { return start.Add(45 * time.Second) }
	svc.poll(ctx)
	if d, _ := st.GetDownload(ctx, got.ID); d.JobID != "job-9" || d.State != store.Queued {
		t.Errorf("found job: %+v", d)
	}
	svc.now = func() time.Time { return start.Add(3 * time.Minute) }
	svc.poll(ctx)
	if d, _ := st.GetDownload(ctx, lost.ID); d.State != store.Failed || !strings.Contains(d.Failure, "SABnzbd has no job") {
		t.Errorf("lost job: %+v", d)
	}
	if len(fake.submitted()) != 0 {
		t.Errorf("submitted %q again after the restart", fake.submitted())
	}
}

// TestCancel stops a download: SABnzbd drops the job, a poll that started
// before the cancel cannot undo it, nothing replaces it, the episode is
// missing rather than failed, and Garner does not pick the release again.
func TestCancel(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	d, err := svc.Best(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != "why German.DL.1080p.BluRay-A" || d.Decisive != 1 {
		t.Errorf("the download lost its release's reasons: %q %d", d.Reasons, d.Decisive)
	}
	fake.progress("job-1", "Downloading", 40)
	svc.poll(ctx)
	stale, _ := st.GetDownload(ctx, d.ID)

	got, err := svc.Cancel(ctx, stale, "cancelled")
	if err != nil || got.State != store.Cancelled {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
	fake.mu.Lock()
	if len(fake.queue) != 0 {
		t.Errorf("SABnzbd still holds %d jobs", len(fake.queue))
	}
	fake.mu.Unlock()
	if err := st.UpdateDownload(ctx, stale, time.Now()); !errors.Is(err, store.ErrCancelled) {
		t.Errorf("a stale poll saved over the cancel: %v", err)
	}
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Cancelled {
		t.Errorf("after the stale save: %s", d.State)
	}
	if _, err := svc.Cancel(ctx, got, "cancelled"); !errors.Is(err, store.ErrNotRunning) {
		t.Errorf("cancelling twice: %v", err)
	}
	if len(fake.submitted()) != 1 {
		t.Errorf("submitted %q; a cancel starts no other release", fake.submitted())
	}
	_, states, _ := st.ShowStatus(ctx, 42, time.Now(), false)
	if i := slices.IndexFunc(states, func(e store.EpisodeStatus) bool { return e.EpisodeID == 101 }); i < 0 || states[i].State == store.EpisodeFailed {
		t.Errorf("episode after a cancel: %+v", states)
	}
	if next, err := svc.Best(ctx, 101); err != nil || next.Title != show+"German.DL.1080p.WEB-B" {
		t.Errorf("next pick = %q, %v; want the next release, not the cancelled one", next.Title, err)
	}
}

func TestRetryable(t *testing.T) {
	for msg, want := range map[string]bool{
		"Aborted, cannot be completed - https://sabnzbd.org/not-complete": true,
		"Repair failed, not enough repair blocks (12 short)":              true,
		"Unpacking failed, archive requires a password":                   true,
		"URL Fetching failed; Server returned 404":                        true,
		"Unpacking failed, write error or disk is full?":                  false,
		"Post-processing was aborted":                                     false,
		"Something SABnzbd has never said":                                false,
	} {
		if got := retryable(msg); got != want {
			t.Errorf("retryable(%q) = %v, want %v", msg, got, want)
		}
	}
}
