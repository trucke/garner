// Package download hands chosen releases to SABnzbd, follows them until
// they finish and replaces a failed release when another one may succeed.
package download

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/trucke/garner/internal/httpx"
	"github.com/trucke/garner/internal/sabnzbd"
	"github.com/trucke/garner/internal/store"
)

const (
	pollActive = 3 * time.Second
	pollIdle   = time.Minute
	// confirmAfter is how long a recorded download may lack SABnzbd's job
	// ID before Garner looks the job up by name.
	confirmAfter = 30 * time.Second
	// giveUpAfter ends the lookup for a job SABnzbd never got.
	giveUpAfter = 2 * time.Minute
	// lostAfter is how many polls a job may be missing from SABnzbd's
	// queue and history before Garner treats it as removed.
	lostAfter = 3
	// maxReplacements bounds how often Garner replaces a failed release on
	// its own. Every failure since the episode's last search counts, manual
	// choices included: many failures in a row suggest a shared problem.
	maxReplacements = 3
	submitTimeout   = time.Minute
)

// ErrNoCandidate means the latest search has no release left to try.
var ErrNoCandidate = errors.New("the latest search has no release left to try; search again")

// Service submits and follows downloads.
type Service struct {
	store    *store.Store
	sab      *sabnzbd.Client
	category string
	log      *slog.Logger
	// Now tells the time: time.Now, or the simulation's clock.
	Now     func() time.Time
	wake    chan struct{}
	missing map[int64]int // polls a job was not found, by download
	// OnChange is called whenever a download changes.
	OnChange func(store.Download)
	// OnCompleted is called once a download finished and its import was
	// queued.
	OnCompleted func(store.Download)
}

// New returns a download service using the SABnzbd category.
func New(st *store.Store, sab *sabnzbd.Client, category string, log *slog.Logger) *Service {
	return &Service{
		store: st, sab: sab, category: category, log: log, Now: time.Now,
		wake: make(chan struct{}, 1), missing: map[int64]int{},
	}
}

// Candidate downloads a candidate chosen by hand, whatever its verdict.
func (s *Service) Candidate(ctx context.Context, candidateID int64) (store.Download, error) {
	c, episodeID, err := s.store.CandidateLink(ctx, candidateID)
	if err != nil {
		return store.Download{}, err
	}
	return s.submit(ctx, episodeID, c, true)
}

// SeasonCandidate downloads a pack of a season search, for its first
// missing episode; the import takes the pack's other episodes along.
func (s *Service) SeasonCandidate(ctx context.Context, candidateID int64) (store.Download, error) {
	c, episodeID, err := s.store.SeasonCandidateLink(ctx, candidateID)
	if err != nil {
		return store.Download{}, err
	}
	return s.submit(ctx, episodeID, c, true)
}

// Best downloads the best release of the episode's latest search that has
// not failed before.
func (s *Service) Best(ctx context.Context, episodeID int64) (store.Download, error) {
	c, err := s.pick(ctx, episodeID, "accept", "fallback")
	if err != nil {
		return store.Download{}, err
	}
	return s.submit(ctx, episodeID, c, false)
}

// Accepted is Best without last resorts: what the monitor downloads on its
// own. A last resort waits for someone to choose it.
func (s *Service) Accepted(ctx context.Context, episodeID int64) (store.Download, error) {
	c, err := s.pick(ctx, episodeID, "accept")
	if err != nil {
		return store.Download{}, err
	}
	return s.submit(ctx, episodeID, c, false)
}

// pick returns the best candidate with one of the verdicts, in their order,
// that has not failed or been cancelled.
func (s *Service) pick(ctx context.Context, episodeID int64, verdicts ...string) (store.Candidate, error) {
	candidates, err := s.store.CandidateLinks(ctx, episodeID)
	if err != nil {
		return store.Candidate{}, err
	}
	failed, err := s.store.FailedTitles(ctx, episodeID)
	if err != nil {
		return store.Candidate{}, err
	}
	for _, verdict := range verdicts {
		for _, c := range candidates {
			if c.Verdict == verdict && !slices.Contains(failed, c.Title) {
				return c, nil
			}
		}
	}
	return store.Candidate{}, ErrNoCandidate
}

// submit records the download, then hands it to SABnzbd. Without an answer
// from SABnzbd the download stays recorded, and the next poll looks the job
// up by name instead of submitting it again.
func (s *Service) submit(ctx context.Context, episodeID int64, c store.Candidate, manual bool) (store.Download, error) {
	d, err := s.store.StartDownload(ctx, store.Download{
		EpisodeID: episodeID, Title: c.Title, Indexer: c.Indexer, Link: c.Link, Size: c.Size,
		Verdict: c.Verdict, Tier: c.Tier, Reasons: c.Reasons, Decisive: c.Decisive, Manual: manual,
	}, s.Now())
	if err != nil {
		return d, err
	}
	s.changed(d)
	// A client that hangs up must not interrupt the handover.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
	defer cancel()
	id, err := s.sab.AddURL(ctx, d.Link, d.Title, s.category)
	var refused *sabnzbd.Error
	switch {
	case err == nil:
		d.JobID, d.State, d.Detail = id, store.Queued, "Queued"
	case errors.As(err, &refused):
		d.State, d.Failure = store.Failed, "SABnzbd did not take the release: "+refused.Message
	default:
		s.log.Warn("SABnzbd did not confirm the download; looking it up later", "download", d.ID, "err", err)
		d.Detail = "waiting for SABnzbd to confirm"
	}
	if err := s.save(ctx, d); err != nil {
		return d, err
	}
	s.log.Info("download submitted", "download", d.ID, "episode", episodeID, "title", d.Title, "manual", manual, "state", d.State)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return d, nil
}

// Run follows running downloads until ctx is cancelled: every few seconds
// while any runs, otherwise once a minute or when a download starts.
func (s *Service) Run(ctx context.Context) {
	for {
		wait := pollIdle
		if s.Poll(ctx) {
			wait = pollActive
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

// poll updates every running download from SABnzbd and reports whether any
// is still running.
func (s *Service) Poll(ctx context.Context) bool {
	active, err := s.store.ActiveDownloads(ctx)
	if err != nil {
		s.log.Error("list running downloads", "err", err)
		return false
	}
	if len(active) == 0 {
		return false
	}
	queue, err := s.sab.Queue(ctx, "")
	if err != nil {
		s.log.Warn("SABnzbd queue unavailable", "err", err)
		return true
	}
	inQueue := map[string]sabnzbd.Job{}
	for _, j := range queue {
		inQueue[j.ID] = j
	}
	var ids []string
	for _, d := range active {
		if _, ok := inQueue[d.JobID]; d.JobID != "" && !ok {
			ids = append(ids, d.JobID)
		}
	}
	inHistory := map[string]sabnzbd.Done{}
	if len(ids) > 0 {
		history, err := s.sab.History(ctx, ids, "")
		if err != nil {
			s.log.Warn("SABnzbd history unavailable", "err", err)
			return true
		}
		for _, h := range history {
			inHistory[h.ID] = h
		}
	}

	running := false
	for _, d := range active {
		before := d
		j, queued := inQueue[d.JobID]
		h, done := inHistory[d.JobID]
		switch {
		case d.JobID == "":
			s.confirm(ctx, &d)
		case queued:
			delete(s.missing, d.ID)
			fromQueue(&d, j)
		case done:
			delete(s.missing, d.ID)
			fromHistory(&d, h)
		default:
			if s.missing[d.ID]++; s.missing[d.ID] >= lostAfter {
				d.State, d.Failure = store.Failed, "SABnzbd no longer lists this download; it was probably removed there"
			}
		}
		if !d.Active() {
			delete(s.missing, d.ID)
		}
		if summary(d) != summary(before) {
			if err := s.save(ctx, d); errors.Is(err, store.ErrCancelled) {
				continue // cancelled while this poll ran
			} else if err != nil {
				s.log.Error("save download", "download", d.ID, "err", err)
				continue
			}
			if d.State == store.Failed {
				s.log.Warn("download failed", "download", d.ID, "title", d.Title, "failure", d.Failure, "retryable", d.Retryable)
				if d.Retryable {
					s.replace(ctx, d)
				}
			} else if d.State == store.Completed {
				s.log.Info("download completed", "download", d.ID, "title", d.Title, "storage", d.Storage)
				if err := s.store.MarkImportPending(ctx, d.ID, s.Now()); err != nil {
					s.log.Error("queue import", "download", d.ID, "err", err)
				} else if s.OnCompleted != nil {
					s.OnCompleted(d)
				}
			}
		}
		running = running || d.Active()
	}
	return running
}

// confirm looks up a download SABnzbd never confirmed, by name.
func (s *Service) confirm(ctx context.Context, d *store.Download) {
	age := s.Now().Sub(d.Created)
	if age < confirmAfter {
		return
	}
	since := d.Created.Add(-time.Minute)
	same := func(name, category string, added time.Time) bool {
		return strings.EqualFold(strings.TrimSuffix(name, ".nzb"), d.Title) && category == s.category && !added.Before(since)
	}
	if queue, err := s.sab.Queue(ctx, d.Title); err == nil {
		for _, j := range queue {
			if same(j.Name, j.Category, j.Added) {
				d.JobID, d.State, d.Detail = j.ID, store.Queued, j.Status
				return
			}
		}
	}
	if history, err := s.sab.History(ctx, nil, d.Title); err == nil {
		for _, h := range history {
			if same(h.Name, h.Category, h.Added) {
				d.JobID = h.ID
				fromHistory(d, h)
				return
			}
		}
	}
	if age >= giveUpAfter {
		d.State, d.Retryable = store.Failed, true
		d.Failure = "SABnzbd has no job for this release; Garner stopped or lost the connection before SABnzbd confirmed it"
	}
}

// Reject marks a completed download's release bad, for example after the
// file check found other contents than the name claimed, and starts the
// next release.
func (s *Service) Reject(ctx context.Context, d store.Download, reason string) {
	d.State, d.Failure, d.Retryable = store.Failed, reason, true
	if err := s.save(ctx, d); err != nil {
		s.log.Error("reject download", "download", d.ID, "err", err)
		return
	}
	s.log.Warn("release rejected after download", "download", d.ID, "title", d.Title, "reason", reason)
	s.replace(ctx, d)
}

// Cancel stops a running download: SABnzbd deletes the job and its files,
// and the download ends as cancelled with reason, without a replacement.
// It returns store.ErrNotRunning when the download ended meanwhile.
func (s *Service) Cancel(ctx context.Context, d store.Download, reason string) (store.Download, error) {
	if d.JobID != "" {
		if err := s.sab.Delete(ctx, d.JobID); err != nil {
			return d, err
		}
	}
	d, err := s.store.CancelDownload(ctx, d.ID, reason, s.Now())
	if err != nil {
		return d, err
	}
	s.log.Info("download cancelled", "download", d.ID, "title", d.Title, "reason", reason)
	s.changed(d)
	return d, nil
}

// replace starts the next release after a failure another release may fix.
// The replacement is never a step down: an accepted release, such as the
// monitor's, is replaced by accepted ones only; last resorts follow only a
// release that was not accepted.
func (s *Service) replace(ctx context.Context, failed store.Download) {
	sr, err := s.store.LoadSearch(ctx, failed.EpisodeID)
	if err != nil {
		s.log.Warn("no search to pick another release from", "episode", failed.EpisodeID, "err", err)
		return
	}
	n, err := s.store.FailedSince(ctx, failed.EpisodeID, sr.Searched)
	if err != nil || n > maxReplacements {
		s.log.Warn("not trying another release", "episode", failed.EpisodeID, "failed", n, "err", err)
		return
	}
	verdicts := []string{"accept", "fallback"}
	if failed.Verdict == "accept" {
		verdicts = verdicts[:1]
	}
	c, err := s.pick(ctx, failed.EpisodeID, verdicts...)
	if err != nil {
		s.log.Info("no other release to try", "episode", failed.EpisodeID, "err", err)
		return
	}
	if _, err := s.submit(ctx, failed.EpisodeID, c, false); err != nil {
		s.log.Warn("could not start the next release", "episode", failed.EpisodeID, "err", err)
	}
}

func (s *Service) save(ctx context.Context, d store.Download) error {
	if err := s.store.UpdateDownload(ctx, d, s.Now()); err != nil {
		return err
	}
	s.changed(d)
	return nil
}

func (s *Service) changed(d store.Download) {
	if s.OnChange != nil {
		s.OnChange(d)
	}
}

func fromQueue(d *store.Download, j sabnzbd.Job) {
	d.State, d.Detail, d.Progress, d.TimeLeft = store.Queued, j.Status, j.Percent, j.TimeLeft
	if j.Status == "Downloading" {
		d.State = store.Downloading
	}
}

// fromHistory applies a history job. SABnzbd's log and failure messages
// quote the download link, which holds Prowlarr's API key, so every URL in
// them is redacted.
func fromHistory(d *store.Download, h sabnzbd.Done) {
	d.TimeLeft = ""
	// A history entry may have failed long before the download finished.
	// Keep its last reported progress instead of inventing completion.
	if h.Status != "Failed" {
		d.Progress = 100
	}
	d.Stages = make([]store.Stage, len(h.Stages))
	for i, st := range h.Stages {
		actions := (store.Stage{Actions: st.Actions}).Lines()
		for j, a := range actions {
			actions[j] = httpx.RedactText(a)
		}
		d.Stages[i] = store.Stage{Name: st.Name, Actions: actions}
	}
	switch h.Status {
	case "Completed":
		d.State, d.Detail, d.Storage = store.Completed, h.Status, h.Storage
	case "Failed":
		d.State, d.Detail = store.Failed, h.Status
		d.Failure = httpx.RedactText(cmp.Or(h.FailMessage, "SABnzbd reports the download as failed without a reason"))
		d.Retryable = retryable(d.Failure)
		d.FailureSummary = d.SummarizeFailure()
	default:
		d.State, d.Detail = store.Processing, cmp.Or(h.Action, h.Status)
	}
}

// summary is what clients see change, with progress in whole percent;
// polls that change nothing are not saved or published.
func summary(d store.Download) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%.0f", d.State, d.Detail, d.JobID, d.TimeLeft, d.Storage, d.Failure, d.Progress)
}

// systemFailures are SABnzbd failures every release would hit, such as a
// full disk; replacing the release cannot help.
var systemFailures = []string{"disk", "no space", "permission", "write error", "post-processing was aborted", "cannot create"}

// releaseFailures are failures of this release that another one may avoid.
var releaseFailures = []string{
	"cannot be completed", "missing articles", "not on your server", "repair failed", "not enough repair blocks",
	"requires a password", "encrypted", "unwanted extension", "fetching failed", "fetch failed", "empty nzb",
	"unusable nzb", "invalid nzb", "crc error",
}

// retryable reports whether another release may succeed where this one
// failed. Unknown failures count as not retryable, so Garner does not
// churn through releases for a problem they share.
func retryable(failure string) bool {
	f := strings.ToLower(failure)
	if slices.ContainsFunc(systemFailures, func(s string) bool { return strings.Contains(f, s) }) {
		return false
	}
	return slices.ContainsFunc(releaseFailures, func(s string) bool { return strings.Contains(f, s) })
}
