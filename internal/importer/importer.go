// Package importer moves finished downloads into the library. It first
// checks that the files hold what the release promised, then moves them on
// the share without copying, and confirms that Jellyfin identified them as
// the intended episodes.
package importer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/trucke/garner/internal/config"
	"github.com/trucke/garner/internal/importer/guard"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/jellyfin"
	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

const (
	pollInterval = 10 * time.Second
	// fullScanAfter is how long Garner waits for Jellyfin to act on its
	// hint before asking for a scan of all libraries. Jellyfin delays its
	// own reaction to file changes by 60 seconds.
	fullScanAfter = 90 * time.Second
	// scanTimeout is how long Garner waits for Jellyfin to list a moved
	// file without progress before it reports the import as unconfirmed.
	scanTimeout = 3 * time.Minute

	detailWaiting  = "moved into the library; waiting for Jellyfin"
	detailFullScan = "moved into the library; Jellyfin did not pick the files up, so Garner asked it to scan all libraries"
)

// ErrNotRetryable means the import has no failed step to repeat.
var ErrNotRetryable = errors.New("only failed or unconfirmed imports can be retried")

// ErrBusy refuses a change to the library's files while another one runs.
var ErrBusy = errors.New("Garner is changing files in the library; try again in a moment")

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true, ".m2ts": true, ".webm": true, ".wmv": true,
}

var sampleName = regexp.MustCompile(`(?i)(^|[^a-z])sample([^a-z]|$)`)

// Prober reads what a file holds.
type Prober interface {
	Probe(ctx context.Context, file string) (inspect.Probe, error)
}

// Options configures the importer.
type Options struct {
	// SABnzbdPaths translates SABnzbd's folders to Garner's view.
	SABnzbdPaths config.PathMap
	// JellyfinPaths translates Jellyfin's folders to Garner's view.
	JellyfinPaths config.PathMap
	Library       config.Library
	// Rules returns a show's reviewed streaming-service order rules.
	Rules func(seriesID int64) map[release.Service]string
	// Profiles are the profiles shows choose from; the zero value holds
	// the built-in ones.
	Profiles selection.Profiles
	// Rename moves files for the guard; nil means os.Rename.
	Rename func(oldpath, newpath string) error
	// Before runs before each change to a file or folder; see
	// guard.Options.Before.
	Before func(op, path string)
}

// Service imports completed downloads.
type Service struct {
	store    *store.Store
	probe    Prober
	jellyfin *jellyfin.Client // nil: imports are not confirmed
	opts     Options
	// guard makes every change to files and folders.
	guard *guard.Guard
	log   *slog.Logger
	// Now tells the time: time.Now, or the simulation's clock.
	Now  func() time.Time
	wake chan struct{}
	// files lets one change to the library's files and their records run at
	// a time: a take-over, a rename, an import's move or deleting a show's
	// files. Each would otherwise act on records another one is changing.
	// Nothing waits for it: a user's request is refused with ErrBusy, and
	// the import loop leaves its move for the next round.
	files sync.Mutex
	// OnChange is called whenever an import changes.
	OnChange func(store.Download)
	// OnRejected marks a download's release bad, so another one is tried.
	OnRejected func(ctx context.Context, d store.Download, reason string)
}

// New returns an importer. jf may be nil.
func New(st *store.Store, probe Prober, jf *jellyfin.Client, opts Options, log *slog.Logger) *Service {
	if opts.Rules == nil {
		opts.Rules = func(int64) map[release.Service]string { return nil }
	}
	g := guard.New(guard.Options{
		Library: opts.Library, SABnzbdPaths: opts.SABnzbdPaths,
		Recorded: st.FileInUse, Planned: st.PlannedRemoval, LibraryFolders: jellyfinFolders(jf, opts.JellyfinPaths),
		Rename: opts.Rename, Before: opts.Before,
	})
	return &Service{store: st, probe: probe, jellyfin: jf, opts: opts, guard: g, log: log, Now: time.Now, wake: make(chan struct{}, 1)}
}

// jellyfinFolders lists Jellyfin's library folders as Garner reaches them;
// nil without Jellyfin. A folder no jellyfin.paths entry covers is out of
// Garner's reach, so it is left out.
func jellyfinFolders(jf *jellyfin.Client, paths config.PathMap) func(context.Context) ([]string, error) {
	if jf == nil {
		return nil
	}
	return func(ctx context.Context) ([]string, error) {
		remote, err := jf.LibraryFolders(ctx)
		if err != nil {
			return nil, err
		}
		var local []string
		for _, r := range remote {
			if l, ok := paths.Local(r); ok {
				local = append(local, l)
			}
		}
		return local, nil
	}
}

// LockFiles reserves the library's files for one change, such as deleting
// a show's files; it fails with ErrBusy while another change runs.
// UnlockFiles ends the change.
func (s *Service) LockFiles() error {
	if !s.files.TryLock() {
		return ErrBusy
	}
	return nil
}

// UnlockFiles ends a change LockFiles began and wakes the import loop for
// a move it left meanwhile.
func (s *Service) UnlockFiles() {
	s.files.Unlock()
	s.Wake()
}

// Wake makes Run look for work now, such as after a download completed.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run imports completed downloads until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	for {
		s.Work(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(pollInterval):
		}
	}
}

func (s *Service) Work(ctx context.Context) {
	s.finishChanges(ctx)
	due, err := s.store.DueImports(ctx)
	if err != nil {
		s.log.Error("list due imports", "err", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		switch d.Import.State {
		case store.ImportPending:
			s.check(ctx, &d)
		case store.ImportMoving:
			s.move(ctx, &d)
		case store.ImportScanning:
			s.verify(ctx, &d)
		}
	}
}

// Retry repeats the step that failed, after the problem was fixed.
func (s *Service) Retry(ctx context.Context, downloadID int64) (store.Download, error) {
	d, err := s.store.GetDownload(ctx, downloadID)
	if err != nil {
		return d, err
	}
	im := d.Import
	switch {
	case im.State == store.ImportFailed && planned(im.Files):
		im.State = store.ImportMoving
	case im.State == store.ImportFailed:
		im.State = store.ImportPending
	case im.State == store.ImportUnverified:
		im.State = store.ImportScanning
	default:
		return d, ErrNotRetryable
	}
	im.Detail, im.Recovery = "retrying", ""
	if im.State == store.ImportScanning {
		im.Detail = detailWaiting
	}
	if err := s.save(ctx, &d, im); err != nil {
		return d, err
	}
	s.Wake()
	return d, nil
}

// check finds the video files, assigns them to episodes and checks that
// they hold what the release promised.
func (s *Service) check(ctx context.Context, d *store.Download) {
	if d.Storage == "" {
		s.fail(ctx, d, nil, "SABnzbd did not say where it put the files", "Check the job in SABnzbd's history, then retry the import.")
		return
	}
	local, ok := s.opts.SABnzbdPaths.Local(d.Storage)
	if !ok {
		s.fail(ctx, d, nil, fmt.Sprintf("SABnzbd put the files in %s, which no sabnzbd.paths entry covers", d.Storage),
			"Add the folder to sabnzbd.paths in garner.toml, restart Garner and retry the import.")
		return
	}
	files, err := videoFiles(local)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.fail(ctx, d, nil, fmt.Sprintf("Garner cannot see %s", local),
			"Check that the media share is mounted and that sabnzbd.paths points at it, then retry the import.")
		return
	case errors.Is(err, fs.ErrPermission):
		s.fail(ctx, d, nil, fmt.Sprintf("Garner may not read %s", local),
			"Give the account Garner uses read access to SABnzbd's completed folder, then retry the import.")
		return
	case err != nil:
		s.fail(ctx, d, nil, err.Error(), "Check the folder, then retry the import.")
		return
	case len(files) == 0:
		s.reject(ctx, d, nil, "the download holds no video file")
		return
	}

	c, err := s.context(ctx, d.EpisodeID)
	if err != nil {
		s.fail(ctx, d, nil, err.Error(), "Retry the import; if this persists, refresh the show.")
		return
	}
	if len(files) == 1 {
		files[0].Episodes = c.releaseEpisodes(d)
	} else {
		var matched []store.ImportFile
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f.Source), filepath.Ext(f.Source))
			if m := c.show.Match(release.Parse(name), time.Time{}); m.Kind == match.Episodes {
				f.Episodes = m.Episodes
				matched = append(matched, f)
			} else {
				s.log.Info("leaving a file it cannot assign", "download", d.ID, "file", f.Source, "reason", m.Reason)
			}
		}
		files = matched
	}
	if !slices.ContainsFunc(files, func(f store.ImportFile) bool { return slices.Contains(f.Episodes, d.EpisodeID) }) {
		s.reject(ctx, d, files, "the download does not hold "+c.show.Label(d.EpisodeID))
		return
	}
	files, notes := s.keepBetter(ctx, d, c, files)

	var problems []string
	for i := range files {
		f := &files[i]
		if fh, err := os.Open(f.Source); err != nil {
			s.fail(ctx, d, files, fmt.Sprintf("Garner may not read %s: %v", filepath.Base(f.Source), unwrapPath(err)),
				"Give the account Garner uses read access to SABnzbd's completed folder, then retry the import.")
			return
		} else {
			fh.Close()
		}
		p, err := s.probe.Probe(ctx, f.Source)
		if errors.Is(err, inspect.ErrUnreadable) {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(f.Source), err))
			continue
		}
		if err != nil {
			s.fail(ctx, d, files, err.Error(), "Check that ffprobe runs (the ffprobe setting in garner.toml), then retry the import.")
			return
		}
		res := inspect.Check(p, c.expect(d, f.Episodes))
		f.Check, _ = json.Marshal(res)
		for _, pr := range res.Problems {
			if len(files) > 1 {
				pr = filepath.Base(f.Source) + ": " + pr
			}
			problems = append(problems, pr)
		}
		f.Target, err = c.target(s.opts.Library, f.Episodes, releaseTags(d.Title, d.Tier, p), filepath.Ext(f.Source))
		if err != nil {
			s.fail(ctx, d, files, err.Error(), "Set library.tv and library.anime in garner.toml, restart Garner and retry the import.")
			return
		}
	}
	if len(problems) > 0 {
		s.reject(ctx, d, files, "file check: "+strings.Join(problems, "; "))
		return
	}
	s.save(ctx, d, store.Import{State: store.ImportMoving, Detail: "the file check passed", Files: files, Notes: notes})
	s.move(ctx, d)
}

// move moves the files into the library, then asks Jellyfin to scan them.
func (s *Service) move(ctx context.Context, d *store.Download) {
	files, notes, ok := s.moveFiles(ctx, d)
	if !ok {
		return
	}
	if s.jellyfin == nil {
		s.save(ctx, d, store.Import{State: store.ImportDone, Detail: "moved into the library; Jellyfin is not configured, so nothing confirmed it", Files: files, Notes: notes})
		return
	}
	var paths []string
	for _, f := range files {
		if p, ok := s.opts.JellyfinPaths.Remote(f.Target); ok {
			paths = append(paths, p)
		}
	}
	if len(paths) < len(files) {
		s.save(ctx, d, store.Import{State: store.ImportUnverified, Detail: "moved into the library, but jellyfin.paths does not cover the library folder",
			Recovery: "Add the library folder to jellyfin.paths in garner.toml, restart Garner and retry the import.", Files: files, Notes: notes})
		return
	}
	if err := s.jellyfin.Created(ctx, paths); err != nil {
		s.log.Warn("could not ask Jellyfin to scan; waiting for its own scan", "err", err)
	}
	s.save(ctx, d, store.Import{State: store.ImportScanning, Detail: detailWaiting, Files: files, Notes: notes})
	s.verify(ctx, d)
}

// moveFiles renames the files into the library and records them. A rename
// moves the file on the share without copying it; a rename that would
// cross filesystems fails instead of copying. Then it removes the pack's
// files whose episodes kept the library's own file. It reports false when
// the move failed, or when another change to the library's files runs:
// then the move stays due, and UnlockFiles wakes the loop for it.
func (s *Service) moveFiles(ctx context.Context, d *store.Download) ([]store.ImportFile, []string, bool) {
	if !s.files.TryLock() {
		return nil, nil, false
	}
	defer s.files.Unlock()
	files := d.Import.Files
	// SABnzbd's folder is looked up only when a file is still in it: after a
	// restart, it may be gone with every file moved already.
	job := sync.OnceValues(func() (guard.Job, error) { return s.guard.Job(d.Storage, d.Title) })
	claim := func(path string) (guard.File, error) {
		j, err := job()
		if err != nil {
			return guard.File{}, err
		}
		return s.guard.InJob(j, path)
	}
	for _, f := range files {
		root := s.rootOf(f.Target)
		if root == "" {
			s.fail(ctx, d, files, f.Target+" is outside the library folders", "Check library.tv and library.anime in garner.toml, then retry the import.")
			return nil, nil, false
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			s.fail(ctx, d, files, fmt.Sprintf("the library folder %s is missing", root),
				"Mount the media share and check library.tv and library.anime in garner.toml, then retry the import. Garner never creates the library folders themselves.")
			return nil, nil, false
		}
		if _, err := os.Stat(f.Source); errors.Is(err, fs.ErrNotExist) {
			// Moved already, before a restart.
			if st, err := os.Stat(f.Target); err == nil && st.Size() == f.Size {
				continue
			}
			s.fail(ctx, d, files, f.Source+" is gone and not in the library", "Check SABnzbd's completed folder; download the episode again if the file was deleted.")
			return nil, nil, false
		}
		if _, err := os.Lstat(f.Target); err == nil {
			ours, err := s.store.FileInUse(ctx, f.Target)
			if err != nil || !ours {
				s.fail(ctx, d, files, f.Target+" already exists and was not imported by Garner",
					"Move or delete that file, then retry the import. Garner never overwrites files it did not import.")
				return nil, nil, false
			}
		}
		source, err := claim(f.Source)
		if err != nil {
			s.fail(ctx, d, files, fmt.Sprintf("Garner may not move %s: %v", filepath.Base(f.Source), err), guardFix)
			return nil, nil, false
		}
		if err := s.guard.Mkdir(filepath.Dir(f.Target)); err != nil {
			fix := permissionFix
			if refused(err) {
				fix = guardFix
			}
			s.fail(ctx, d, files, fmt.Sprintf("cannot create %s: %v", filepath.Dir(f.Target), unwrapPath(err)), fix)
			return nil, nil, false
		}
		if err := s.guard.Move(ctx, source, f.Target); err != nil {
			switch {
			case refused(err):
				s.fail(ctx, d, files, fmt.Sprintf("Garner may not move %s: %v", filepath.Base(f.Source), err), guardFix)
			case errors.Is(err, syscall.EXDEV):
				s.fail(ctx, d, files, "the download and the library are on different filesystems, so the file would have to be copied",
					"Keep SABnzbd's completed folder and the library on the same share and dataset, then retry the import. Garner moves files; it does not copy them.")
			case errors.Is(err, fs.ErrPermission):
				s.fail(ctx, d, files, fmt.Sprintf("Garner may not move %s", filepath.Base(f.Source)), permissionFix)
			default:
				s.fail(ctx, d, files, fmt.Sprintf("moving %s failed: %v", filepath.Base(f.Source), unwrapPath(err)), "Check both folders, then retry the import.")
			}
			return nil, nil, false
		}
		s.log.Info("imported file", "download", d.ID, "target", f.Target)
	}
	if err := s.recordFiles(ctx, d, files); err != nil {
		s.fail(ctx, d, files, "the files are in the library, but recording them failed: "+err.Error(), "Retry the import.")
		return nil, nil, false
	}
	s.removeKept(ctx, d, job)
	notes := append(slices.Clone(d.Import.Notes), s.tidy(ctx, d, files)...)
	if notes == nil {
		notes = []string{}
	}
	return files, notes, true
}

const permissionFix = "On the NAS, give the account Garner uses write access to SABnzbd's completed folder and the library (UID 1000 in this setup), then retry the import."

// guardFix is the recovery step when the guard refused to change a file
// that Garner does not own.
const guardFix = "Check sabnzbd.paths, jellyfin.paths, library.tv and library.anime in garner.toml, then retry the import. Garner changes only files and folders it owns."

// refused reports whether the guard refused a change.
func refused(err error) bool {
	var r *guard.Refusal
	return errors.As(err, &r)
}

// keepBetter drops a pack's files for episodes, other than the one it was
// chosen for, whose file came from a release of the same tier or a better
// one: one version per episode, the better one. It returns the files to
// import and a note naming the episodes that kept their file; removeKept
// deletes the dropped files after the move.
func (s *Service) keepBetter(ctx context.Context, d *store.Download, c showContext, files []store.ImportFile) (keep []store.ImportFile, notes []string) {
	if len(files) < 2 {
		return files, nil
	}
	have, err := s.store.SeriesFileTiers(ctx, c.data.Series.ID)
	if err != nil {
		s.log.Warn("could not compare with the files in the library; importing every file", "download", d.ID, "err", err)
		return files, nil
	}
	var kept []int64
	for _, f := range files {
		if !libraryKeeps(c, d, have, f.Episodes) {
			keep = append(keep, f)
			continue
		}
		kept = append(kept, f.Episodes...)
	}
	if len(kept) == 0 {
		return keep, nil
	}
	s.log.Info("kept the existing files", "download", d.ID, "episodes", c.show.Describe(kept))
	return keep, []string{"kept the existing file of " + c.show.Describe(kept) + ": as good as the pack's or better"}
}

// libraryKeeps reports whether the library keeps its own files of a
// pack's episodes: none of them is the episode the download is for, and
// each has a file from a release as good as the pack's or better.
func libraryKeeps(c showContext, d *store.Download, have map[int64]string, episodes []int64) bool {
	if len(episodes) == 0 || slices.Contains(episodes, d.EpisodeID) {
		return false
	}
	pack := selection.TierRank(c.profile, d.Tier)
	for _, ep := range episodes {
		if tier, found := have[ep]; !found || selection.TierRank(c.profile, tier) > pack {
			return false
		}
	}
	return true
}

// removeKept deletes the videos left in SABnzbd's folder after a move
// whose episodes the library keeps its own files of, as keepBetter decides.
// It works them out from the folder instead of remembering check's list,
// so a retry or a restart after a failed move removes them as well. Videos
// it cannot assign stay for tidy to report.
func (s *Service) removeKept(ctx context.Context, d *store.Download, job func() (guard.Job, error)) {
	j, err := job()
	if err != nil {
		return // gone already, or not Garner's to change
	}
	videos, err := videoFiles(j.Dir())
	if err != nil || len(videos) == 0 {
		return
	}
	c, err := s.context(ctx, d.EpisodeID)
	if err != nil {
		return
	}
	have, err := s.store.SeriesFileTiers(ctx, c.data.Series.ID)
	if err != nil {
		return
	}
	for _, f := range videos {
		name := strings.TrimSuffix(filepath.Base(f.Source), filepath.Ext(f.Source))
		m := c.show.Match(release.Parse(name), time.Time{})
		if m.Kind != match.Episodes || !libraryKeeps(c, d, have, m.Episodes) {
			continue
		}
		claimed, err := s.guard.InJob(j, f.Source)
		if err == nil {
			err = s.guard.Remove(claimed)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("could not remove a file it does not import", "download", d.ID, "file", f.Source, "err", err)
		}
	}
}

// recordFiles records each episode's file and removes the version Garner
// imported or took over before: one version per episode. A record and the
// plan to remove the version it replaces change in one step, so a crash
// before the removal leaves the plan for finishChanges.
func (s *Service) recordFiles(ctx context.Context, d *store.Download, files []store.ImportFile) error {
	// The previous versions are claimed while their records still point at
	// them.
	claims := map[string]guard.File{}
	var records []store.EpisodeFile
	for _, f := range files {
		for _, ep := range f.Episodes {
			prev, found, err := s.store.EpisodeFile(ctx, ep)
			if err == nil && found && prev.Path != f.Target {
				if c, err := s.guard.Recorded(ctx, prev.Path); err != nil {
					s.log.Warn("left the previous version in place", "path", prev.Path, "err", err)
				} else {
					claims[c.Path()] = c
				}
			}
			records = append(records, store.EpisodeFile{EpisodeID: ep, Path: f.Target, Size: f.Size, Tier: d.Tier})
		}
	}
	planned, err := s.store.RecordFiles(ctx, records, d.ID, s.Now(), slices.Sorted(maps.Keys(claims)))
	if err != nil {
		return err
	}
	for _, p := range planned {
		if err := s.removeVersion(ctx, claims[p]); err != nil {
			s.log.Warn("could not remove the previous version yet; Garner tries again later", "path", p, "err", err)
		}
	}
	return nil
}

// removeVersion removes a version of an episode's file that no record uses
// any more, with its subtitle files, and drops the plan to remove it. When
// the removal fails, as in a folder that refuses writes or one that is
// away because the share is not mounted, the plan stays, and
// finishChanges tries again in later rounds without saying so.
func (s *Service) removeVersion(ctx context.Context, c guard.File) error {
	switch err := s.guard.Remove(c); {
	case err == nil:
		s.guard.RemoveSubtitles(c)
		s.log.Info("removed the previous version", "path", c.Path())
	case !errors.Is(err, fs.ErrNotExist):
		return err
	case !dirExists(filepath.Dir(c.Path())):
		return err // not gone: its folder is away
	}
	if err := s.store.DropFileChange(ctx, c.Path()); err != nil {
		s.log.Warn("could not drop the plan to remove a file", "path", c.Path(), "err", err)
	}
	return nil
}

// finishChanges finishes the changes to files that a crash or a failed
// removal left, as their plans in the database tell: a file that moved
// gets its record moved too, one that did not keeps its record, and a
// version an upgrade replaced goes. A change waits while the folder it
// happens in is missing, as when the share is not mounted. It runs only
// while no other change to the library's files runs, so every plan it
// finds is one left that way.
func (s *Service) finishChanges(ctx context.Context) {
	changes, err := s.store.FileChanges(ctx)
	if err != nil {
		s.log.Error("list planned changes to files", "err", err)
		return
	}
	if len(changes) == 0 || !s.files.TryLock() {
		return
	}
	defer s.files.Unlock()
	for _, c := range changes {
		if c.NewPath != "" {
			if !dirExists(filepath.Dir(c.NewPath)) {
				continue
			}
			if _, err := os.Lstat(c.NewPath); err != nil {
				// It did not move, so its record is right.
				if err := s.store.DropFileChange(ctx, c.Path); err != nil {
					s.log.Warn("could not drop the plan to move a file", "path", c.Path, "err", err)
				}
			} else if err := s.store.FinishMove(ctx, c.Path, c.NewPath); err != nil {
				s.log.Warn("could not record an earlier move", "from", c.Path, "to", c.NewPath, "err", err)
			} else {
				s.log.Info("recorded an earlier move", "from", c.Path, "to", c.NewPath)
			}
			continue
		}
		if !dirExists(filepath.Dir(c.Path)) {
			continue
		}
		used, err := s.store.FileInUse(ctx, c.Path)
		if err != nil {
			s.log.Warn("could not check a file planned for removal", "path", c.Path, "err", err)
			continue
		}
		if used {
			// Recorded again meanwhile: it stays.
			if err := s.store.DropFileChange(ctx, c.Path); err != nil {
				s.log.Warn("could not drop the plan to remove a file", "path", c.Path, "err", err)
			}
			continue
		}
		claim, err := s.guard.Planned(ctx, c.Path)
		if err != nil {
			s.log.Warn("could not claim a file planned for removal", "path", c.Path, "err", err)
			continue
		}
		s.removeVersion(ctx, claim) // a failure was told the first time
	}
}

func dirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// verify checks that Jellyfin lists each file as the intended episode.
func (s *Service) verify(ctx context.Context, d *store.Download) {
	if s.jellyfin == nil {
		// Jellyfin was configured when the files were moved, but no longer is.
		s.save(ctx, d, store.Import{State: store.ImportDone, Detail: "moved into the library; Jellyfin is not configured, so nothing confirmed it", Files: d.Import.Files})
		return
	}
	c, err := s.context(ctx, d.EpisodeID)
	if err != nil {
		s.log.Warn("verify import", "download", d.ID, "err", err)
		return
	}
	files := slices.Clone(d.Import.Files)
	progress, done, listed := false, true, false
	for i := range files {
		f := &files[i]
		if f.Verified {
			continue
		}
		jfFile, _ := s.opts.JellyfinPaths.Remote(f.Target)
		jfShow, _ := s.opts.JellyfinPaths.Remote(filepath.Dir(filepath.Dir(f.Target)))
		ep, found, err := s.jellyfin.EpisodeAt(ctx, jfShow, jfFile)
		if err != nil {
			s.log.Warn("Jellyfin lookup failed", "download", d.ID, "err", err)
			done = false
			continue
		}
		if !found {
			done = false
			continue
		}
		listed = true
		if ep.TVDB == "" {
			// Jellyfin lists a file before its metadata provider has
			// identified it; wait for the TVDB ID.
			done = false
			continue
		}
		progress = true
		want := c.episodes[f.Episodes[0]]
		f.Jellyfin = fmt.Sprintf("S%02dE%02d %q, TVDB episode %s", ep.Season, ep.Number, ep.Name, cmp.Or(ep.TVDB, "none"))
		if ep.TVDB != strconv.FormatInt(want.ID, 10) || ep.SeriesTVDB != strconv.FormatInt(c.data.Series.ID, 10) {
			s.save(ctx, d, store.Import{State: store.ImportUnverified, Files: files,
				Detail: fmt.Sprintf("Jellyfin identified %s as %s (show TVDB %s), not %s (TVDB episode %d)",
					filepath.Base(f.Target), f.Jellyfin, cmp.Or(ep.SeriesTVDB, "none"), c.show.Label(want.ID), want.ID),
				Recovery: "Correct the match in Jellyfin (Identify on the show or episode), then retry the import so Garner checks again."})
			return
		}
		f.Verified = true
	}
	switch {
	case done:
		labels := make([]string, len(files))
		for i, f := range files {
			labels[i] = f.Jellyfin
		}
		s.save(ctx, d, store.Import{State: store.ImportDone, Detail: "Jellyfin lists it as " + strings.Join(labels, "; "), Files: files})
	case progress:
		s.save(ctx, d, store.Import{State: store.ImportScanning, Detail: d.Import.Detail, Files: files})
	case !listed && d.Import.Detail == detailWaiting && s.Now().Sub(d.Import.Updated) > fullScanAfter:
		if err := s.jellyfin.RefreshLibrary(ctx); err != nil {
			s.log.Warn("could not ask Jellyfin to scan all libraries", "err", err)
			return
		}
		s.save(ctx, d, store.Import{State: store.ImportScanning, Detail: detailFullScan, Files: files})
	case s.Now().Sub(d.Import.Updated) > scanTimeout && listed:
		s.save(ctx, d, store.Import{State: store.ImportUnverified, Files: files,
			Detail:   fmt.Sprintf("Jellyfin lists the files but has not identified them through TVDB after %s", scanTimeout),
			Recovery: "Check that the TheTVDB plugin comes first for this library in Jellyfin, refresh the show's metadata, then retry the import."})
	case s.Now().Sub(d.Import.Updated) > scanTimeout:
		s.save(ctx, d, store.Import{State: store.ImportUnverified, Files: files,
			Detail:   fmt.Sprintf("the files are in the library, but Jellyfin has not listed them after %s", scanTimeout),
			Recovery: "Scan the library in Jellyfin, then retry the import so Garner checks again."})
	}
}

// save stores an import's progress, keeping earlier notes unless im brings
// its own. Callers in the loop may ignore the error: it is logged, and the
// step runs again on the next round.
func (s *Service) save(ctx context.Context, d *store.Download, im store.Import) error {
	if im.Notes == nil {
		im.Notes = d.Import.Notes
	}
	if err := s.store.UpdateImport(ctx, d.ID, im, s.Now()); err != nil {
		s.log.Error("save import", "download", d.ID, "err", err)
		return err
	}
	im.Updated = s.Now()
	d.Import = im
	s.log.Info("import", "download", d.ID, "state", im.State, "detail", im.Detail)
	if s.OnChange != nil {
		s.OnChange(*d)
	}
	return nil
}

func (s *Service) fail(ctx context.Context, d *store.Download, files []store.ImportFile, detail, recovery string) {
	s.save(ctx, d, store.Import{State: store.ImportFailed, Detail: detail, Recovery: recovery, Files: files})
}

// reject marks the release bad, removes its files from SABnzbd's folder and
// lets the download service try the next release.
func (s *Service) reject(ctx context.Context, d *store.Download, files []store.ImportFile, reason string) {
	s.save(ctx, d, store.Import{State: store.ImportRejected, Detail: reason, Files: files})
	s.discard(ctx, d)
	if s.OnRejected != nil {
		s.OnRejected(ctx, *d, reason)
	}
}

func (s *Service) rootOf(target string) string {
	for _, root := range []string{s.opts.Library.TV, s.opts.Library.Anime} {
		if root != "" && strings.HasPrefix(target, filepath.Clean(root)+string(filepath.Separator)) {
			return filepath.Clean(root)
		}
	}
	return ""
}

// showContext is a show prepared for one import.
type showContext struct {
	data     store.SeriesData
	show     *match.Show
	profile  selection.Profile
	episodes map[int64]store.Episode
}

func (s *Service) context(ctx context.Context, episodeID int64) (showContext, error) {
	seriesID, err := s.store.SeriesOfEpisode(ctx, episodeID)
	if err != nil {
		return showContext{}, err
	}
	return s.seriesContext(ctx, seriesID)
}

func (s *Service) seriesContext(ctx context.Context, seriesID int64) (showContext, error) {
	data, err := s.store.LoadSeries(ctx, seriesID)
	if err != nil {
		return showContext{}, err
	}
	profile := s.opts.Profiles.Named(data.Series.Profile)
	c := showContext{
		data: data, profile: profile, episodes: map[int64]store.Episode{},
		show: match.NewShow(data, match.Options{Anime: profile.Name == selection.AnimeName, ServiceOrders: s.opts.Rules(seriesID)}),
	}
	for _, e := range data.Episodes {
		c.episodes[e.ID] = e
	}
	return c, nil
}

// releaseEpisodes are the episodes the release name claims, or the wanted
// episode when the name says nothing usable.
func (c showContext) releaseEpisodes(d *store.Download) []int64 {
	if m := c.show.Match(release.Parse(d.Title), time.Time{}); m.Kind == match.Episodes {
		return m.Episodes
	}
	return []int64{d.EpisodeID}
}

// expect states what one file must hold: the tier's language, or any of
// the profile's for a last resort; for a release chosen by hand, a missing
// language is only noted.
func (c showContext) expect(d *store.Download, episodes []int64) inspect.Expect {
	e := inspect.Expect{Strict: !d.Manual}
	if f := strings.Fields(d.Tier); len(f) == 2 {
		if l, ok := release.LanguageNamed(f[0]); ok {
			e.Languages = []release.Language{l}
		}
		if r, err := strconv.Atoi(strings.TrimSuffix(f[1], "p")); err == nil {
			e.Resolution = release.Resolution(r)
		}
	}
	if len(e.Languages) == 0 {
		for _, t := range c.profile.Tiers {
			if !slices.Contains(e.Languages, t.Language) {
				e.Languages = append(e.Languages, t.Language)
			}
		}
	}
	for _, id := range episodes {
		e.Runtime += time.Duration(cmp.Or(c.episodes[id].Runtime, c.data.Series.AverageRuntime)) * time.Minute
	}
	return e
}

// videoFiles lists the video files of a download folder, without samples
// and extras far smaller than the main file.
func videoFiles(dir string) ([]store.ImportFile, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	var files []store.ImportFile
	// A sample's file name or folder name says "sample".
	add := func(p string, size int64) {
		rel := filepath.Base(p)
		if st.IsDir() {
			rel, _ = filepath.Rel(dir, p)
		}
		if videoExtensions[strings.ToLower(filepath.Ext(p))] && !sampleName.MatchString(rel) {
			files = append(files, store.ImportFile{Source: p, Size: size})
		}
	}
	if !st.IsDir() {
		add(dir, st.Size())
	} else if err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		add(p, info.Size())
		return nil
	}); err != nil {
		return nil, err
	}
	var largest int64
	for _, f := range files {
		largest = max(largest, f.Size)
	}
	files = slices.DeleteFunc(files, func(f store.ImportFile) bool { return f.Size < largest/10 })
	slices.SortFunc(files, func(a, b store.ImportFile) int { return strings.Compare(a.Source, b.Source) })
	return files, nil
}

func planned(files []store.ImportFile) bool {
	return len(files) > 0 && !slices.ContainsFunc(files, func(f store.ImportFile) bool { return f.Target == "" })
}

// unwrapPath drops the path from an *fs.PathError; the message names it.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}
