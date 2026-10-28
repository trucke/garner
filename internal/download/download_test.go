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
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Downloading || d.Progress != 40 {
		t.Errorf("after progress: %+v", d)
	}

	// Missing articles: another release may succeed, so B starts.
	fake.finish("job-1", "Failed", "Aborted, cannot be completed - https://sabnzbd.org/not-complete", "")
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Failed || !d.Retryable || d.Progress != 40 {
		t.Errorf("after missing articles: state %s, retryable %v, progress %.0f; want failed, retryable and the last reported 40%%", d.State, d.Retryable, d.Progress)
	}
	if got := fake.submitted(); len(got) != 2 || got[1] != show+"German.DL.1080p.WEB-B" {
		t.Fatalf("submitted %q; want the next release", got)
	}

	// A full disk would stop any release, so nothing new starts.
	fake.finish("job-2", "Failed", "Unpacking failed, write error or disk is full?", "")
	svc.Poll(ctx)
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
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, d.ID); d.State != store.Completed || d.Storage != "/data/usenet/complete/tv/x" {
		t.Errorf("after completion: %+v", d)
	}
	all, _ := json.Marshal(published)
	if strings.Contains(string(all), "SECRET") {
		t.Error("a published download holds the download link's API key")
	}
}

// TestFailureEvidence checks the live event as well as the stored record:
// HTML must be decoded before credentials are redacted, and a concrete
// reason must not replace SABnzbd's original failure report.
func TestFailureEvidence(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	var event store.Download
	d, err := svc.Best(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	const failure = "Aborted, cannot be completed - https://sabnzbd.org/not-complete"
	fake.finish(d.JobID, "Failed", failure, "")
	fake.mu.Lock()
	fake.history[0]["stage_log"] = []map[string]any{
		{"name": "Source", "actions": []string{"https://indexer.invalid/download?file=1&amp;apikey=SECRET"}},
		{"name": "Download", "actions": []string{"Downloaded in 12 seconds<br/>27 articles were missing"}},
	}
	fake.mu.Unlock()
	// Keep this job's failure event, not the replacement's submission.
	svc.OnChange = func(next store.Download) {
		if next.ID == d.ID {
			event = next
		}
	}
	svc.Poll(ctx)
	stored, err := st.GetDownload(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]store.Download{"event": event, "stored": stored} {
		if got.FailureSummary != "27 articles missing" || got.Failure != failure || got.Progress != 0 {
			t.Errorf("%s lost the concrete reason, original report or last known progress", name)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "SECRET") {
			t.Errorf("%s exposes a credential", name)
		}
		for _, stage := range got.Stages {
			for _, action := range stage.Actions {
				if strings.Contains(action, "<br") || strings.Contains(action, "&amp;") {
					t.Errorf("%s exposes raw HTML", name)
				}
			}
		}
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
	svc.Poll(ctx)
	fake.finish("job-2", "Failed", "Aborted, cannot be completed", "")
	svc.Poll(ctx)
	if got := fake.submitted(); len(got) != 2 || got[1] != show+"German.DL.1080p.WEB-B" {
		t.Fatalf("submitted %q; want A, then B, and no last resort", got)
	}
}

// TestSameTitlePostings fails one of two postings of the same release: the
// other is tried before a release of a lower preference, the failed one
// stays out after a new search, and the search shows each posting's own
// attempt (GRR-43).
func TestSameTitlePostings(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	const german = show + "German.DL.1080p.WEB-GRP"
	search := func(links string) {
		t.Helper()
		var candidates []store.Candidate
		for _, c := range []struct {
			title, posting string
			size           int64
		}{{german, "p1", 725}, {german, "p2", 698}, {show + "1080p.WEB-GRP", "p3", 900}} {
			candidates = append(candidates, store.Candidate{Indexer: "One", Title: c.title, Posting: c.posting, Size: c.size,
				Verdict: "accept", Link: "http://prowlarr/1/download?apikey=SECRET&" + links + c.posting,
				Page: "https://indexer.invalid/details/" + c.posting})
		}
		if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: time.Now().Add(-time.Minute), Outcome: "found",
			Candidates: candidates}); err != nil {
			t.Fatal(err)
		}
	}
	search("first")

	if d, err := svc.Best(ctx, 101); err != nil || d.Posting != "p1" {
		t.Fatalf("Best = %+v, %v; want posting p1", d, err)
	}
	fake.finish("job-1", "Failed", "Aborted, cannot be completed", "")
	svc.Poll(ctx)
	if got := fake.submitted(); len(got) != 2 || got[1] != german {
		t.Fatalf("submitted %q; want the other posting of %s next", got, german)
	}
	sr, err := st.LoadSearch(ctx, 101)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Candidates[1].Page != "https://indexer.invalid/details/p2" {
		t.Errorf("page = %q after saving the search", sr.Candidates[1].Page)
	}
	var states []string
	for _, c := range sr.Candidates {
		state := "untried"
		if c.Attempt != nil {
			state = c.Attempt.State
		}
		states = append(states, state)
	}
	if want := []string{store.Failed, store.Queued, "untried"}; !slices.Equal(states, want) {
		t.Errorf("attempts by posting = %q, want %q", states, want)
	}

	// A new search brings new links; the failed posting stays out.
	fake.finish("job-2", "Completed", "", "/data/usenet/complete/tv/x")
	svc.Poll(ctx)
	search("second")
	if d, err := svc.Best(ctx, 101); err != nil || d.Posting != "p2" {
		t.Errorf("Best after a new search = %+v, %v; want posting p2", d, err)
	}
}

// TestEarlierFailuresWithoutPosting covers downloads from before Garner
// kept postings apart: one excludes the postings of its name and size only.
func TestEarlierFailuresWithoutPosting(t *testing.T) {
	ctx := context.Background()
	st, svc, _ := setup(t)
	const german = show + "German.DL.1080p.WEB-GRP"
	d, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: german, Size: 725, Verdict: "accept", Link: "x"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d.State, d.Failure = store.Failed, "Aborted, cannot be completed"
	if err := st.UpdateDownload(ctx, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSearch(ctx, store.Search{EpisodeID: 101, Searched: time.Now(), Outcome: "found", Candidates: []store.Candidate{
		{Indexer: "One", Title: german, Posting: "p1", Size: 725, Verdict: "accept", Link: "y"},
		{Indexer: "One", Title: german, Posting: "p2", Size: 698, Verdict: "accept", Link: "z"},
	}}); err != nil {
		t.Fatal(err)
	}
	if d, err := svc.Best(ctx, 101); err != nil || d.Posting != "p2" {
		t.Errorf("Best = %+v, %v; want posting p2", d, err)
	}
}

// TestRestart covers a crash between recording a download and SABnzbd's
// answer: the job is found by name instead of submitted again, and a job
// SABnzbd never got ends as failed.
func TestRestart(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	start := time.Now()
	svc.Now = func() time.Time { return start }
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

	svc.Now = func() time.Time { return start.Add(45 * time.Second) }
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, got.ID); d.JobID != "job-9" || d.State != store.Queued {
		t.Errorf("found job: %+v", d)
	}
	svc.Now = func() time.Time { return start.Add(3 * time.Minute) }
	svc.Poll(ctx)
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
	svc.Poll(ctx)
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
	_, states, _ := st.ShowStatus(ctx, 42, time.Now(), nil)
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

// TestConfirmLeavesOtherJobs confirms a download SABnzbd never answered
// for while another download of the same release, such as the same season
// pack for another episode, has its job: the job is not the first one's,
// so it gives up as without a job (GRR-46).
func TestConfirmLeavesOtherJobs(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	start := time.Now()
	svc.Now = func() time.Time { return start }
	const pack = "Harbor.Precinct.S01.German.DL.1080p.WEB-P"
	other, err := st.StartDownload(ctx, store.Download{EpisodeID: 102, Title: pack, Link: "http://prowlarr/1/download?apikey=SECRET", Verdict: "accept"}, start)
	if err != nil {
		t.Fatal(err)
	}
	other.JobID, other.State = "job-7", store.Queued
	if err := st.UpdateDownload(ctx, other, start); err != nil {
		t.Fatal(err)
	}
	fake.queue = append(fake.queue, map[string]any{"nzo_id": "job-7", "filename": pack, "cat": "tv",
		"status": "Downloading", "percentage": "10", "timeleft": "0:01:00", "time_added": start.Unix()})
	lost, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: pack, Link: "http://prowlarr/1/download?apikey=SECRET", Verdict: "accept"}, start)
	if err != nil {
		t.Fatal(err)
	}

	svc.Now = func() time.Time { return start.Add(45 * time.Second) }
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, lost.ID); d.JobID != "" {
		t.Errorf("took the other download's job: %+v", d)
	}
	svc.Now = func() time.Time { return start.Add(3 * time.Minute) }
	svc.Poll(ctx)
	if d, _ := st.GetDownload(ctx, lost.ID); d.State != store.Failed || !strings.Contains(d.Failure, "SABnzbd has no job") {
		t.Errorf("lost job: %+v", d)
	}
	if d, _ := st.GetDownload(ctx, other.ID); d.JobID != "job-7" || !d.Active() {
		t.Errorf("the other download: %+v", d)
	}
}

// TestConfirmWaitsWhenUnsure confirms a download after the time to give up
// while the database cannot tell whether another download holds the job
// SABnzbd lists: it tries again later rather than fail (GRR-46).
func TestConfirmWaitsWhenUnsure(t *testing.T) {
	ctx := context.Background()
	st, svc, fake := setup(t)
	start := time.Now()
	d, err := st.StartDownload(ctx, store.Download{EpisodeID: 101, Title: show + "German.DL.1080p.WEB-B", Link: "x", Verdict: "accept"}, start)
	if err != nil {
		t.Fatal(err)
	}
	fake.queue = append(fake.queue, map[string]any{"nzo_id": "job-3", "filename": d.Title, "cat": "tv",
		"status": "Downloading", "percentage": "10", "timeleft": "0:01:00", "time_added": start.Unix()})
	st.Close() // every check fails
	svc.Now = func() time.Time { return start.Add(3 * time.Minute) }
	svc.confirm(ctx, &d)
	if d.State != store.Submitting || d.JobID != "" {
		t.Errorf("after a failed check: %s, job %q, %q", d.State, d.JobID, d.Failure)
	}
}
