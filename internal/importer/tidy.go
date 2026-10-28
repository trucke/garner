package importer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trucke/garner/internal/importer/guard"
	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// tidy runs after the videos were moved: it puts the release's subtitle
// files next to their videos and removes the rest of SABnzbd's job folder.
// It never fails the import; what stays behind is returned as notes.
func (s *Service) tidy(ctx context.Context, d *store.Download, files []store.ImportFile) []string {
	job, err := s.guard.Job(d.Storage, d.Title)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed already, before a restart
	}
	if err == nil && job.KeepReason() != "" {
		err = errors.New(job.KeepReason())
	}
	if err != nil {
		return []string{"SABnzbd's folder was left in place: " + err.Error()}
	}
	dir := job.Dir()
	notes := s.moveSubtitles(ctx, dir, files, false, func(_ store.ImportFile, p string) (guard.File, error) {
		return s.guard.InJob(job, p)
	})
	remaining, err := videoFiles(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notes
	case err != nil:
		return append(notes, fmt.Sprintf("SABnzbd's folder was left in place: %v", unwrapPath(err)))
	case len(remaining) > 0:
		return append(notes, fmt.Sprintf("SABnzbd's folder %s was left in place: %s", dir, s.unassigned(ctx, d, remaining)))
	}
	if subs := subtitleFiles(dir); subs > 0 {
		return append(notes, fmt.Sprintf("SABnzbd's folder %s was left in place: it holds %s that could not be moved", dir, count(subs, "subtitle file")))
	}
	if err := s.guard.RemoveJob(ctx, job); refused(err) {
		return append(notes, fmt.Sprintf("SABnzbd's folder %s was left in place: %v", dir, err))
	} else if err != nil {
		s.log.Warn("could not remove SABnzbd's folder", "download", d.ID, "dir", dir, "err", err)
		return append(notes, fmt.Sprintf("SABnzbd's folder %s could not be removed: %v", dir, unwrapPath(err)))
	}
	s.log.Info("removed SABnzbd's folder", "download", d.ID, "dir", dir)
	return notes
}

// unassigned says which videos the import left because no episode could be
// matched to them, and why, as their names tell. The import logged the same
// reasons; reading the names again also covers a retry or a restart.
func (s *Service) unassigned(ctx context.Context, d *store.Download, files []store.ImportFile) string {
	const shown = 3
	c, err := s.context(ctx, d.EpisodeID)
	var named []string
	for i, f := range files {
		if i == shown {
			named = append(named, fmt.Sprintf("and %d more", len(files)-shown))
			break
		}
		name := filepath.Base(f.Source)
		if err == nil {
			m := c.show.Match(release.Parse(strings.TrimSuffix(name, filepath.Ext(name))), time.Time{})
			if m.Kind != match.Episodes && m.Reason != "" {
				name += ": " + m.Reason
			}
		}
		named = append(named, name)
	}
	return fmt.Sprintf("it holds %s that could not be assigned to an episode (%s)",
		count(len(files), "video file"), strings.Join(named, "; "))
}

// discard removes the job folder of a rejected release.
func (s *Service) discard(ctx context.Context, d *store.Download) {
	job, err := s.guard.Job(d.Storage, d.Title)
	if err == nil {
		err = s.guard.RemoveJob(ctx, job)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Removed already.
	case refused(err):
		s.log.Warn("left the rejected release's files in place", "download", d.ID, "reason", err)
	case err != nil:
		s.log.Warn("could not remove the rejected release's files", "download", d.ID, "dir", job.Dir(), "err", err)
	default:
		s.log.Info("removed the rejected release's files", "download", d.ID, "dir", job.Dir())
	}
}

// subtitleFiles counts the subtitle files left in dir.
func subtitleFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && guard.IsSubtitle(p) {
			n++
		}
		return nil
	})
	return n
}

// moveSubtitles moves the subtitle files in dir next to the videos they
// belong to, named the way Jellyfin reads external subtitles:
// "<video name>.<language>[.forced][.sdh].<ext>". A subtitle's name must
// start with the video's original name, except in a download's folder with
// one video, where every subtitle file belongs to it. shared means dir holds
// other videos too, such as a library's season folder, so names decide.
// claim asks the guard for a subtitle file of its video.
func (s *Service) moveSubtitles(ctx context.Context, dir string, files []store.ImportFile, shared bool,
	claim func(video store.ImportFile, subtitle string) (guard.File, error)) []string {
	var notes []string
	moved := 0
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !guard.IsSubtitle(p) {
			return nil
		}
		base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		var owner *store.ImportFile
		rest, longest := base, 0
		for i := range files {
			f := &files[i]
			video := strings.TrimSuffix(filepath.Base(f.Source), filepath.Ext(f.Source))
			// The video's name must end at a separator, so "Episode10.en"
			// does not belong to "Episode1".
			r, ok := cutPrefixFold(base, video)
			if ok && (r == "" || strings.ContainsRune("._- ", rune(r[0]))) && len(video) > longest {
				owner, rest, longest = f, r, len(video)
			}
		}
		if owner == nil && len(files) == 1 && !shared {
			owner = &files[0]
		}
		if owner == nil || owner.Target == "" {
			return nil
		}
		target := subtitleTarget(owner.Target, rest, strings.ToLower(filepath.Ext(p)))
		if target == "" {
			notes = append(notes, fmt.Sprintf("subtitle %s was left behind: its name is taken next to the video", filepath.Base(p)))
			return nil
		}
		sub, err := claim(*owner, p)
		if err == nil {
			err = s.guard.Move(ctx, sub, target)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("subtitle %s could not be moved: %v", filepath.Base(p), unwrapPath(err)))
			return nil
		}
		moved++
		return nil
	})
	if moved > 0 {
		notes = append([]string{fmt.Sprintf("moved %s next to the video", count(moved, "subtitle file"))}, notes...)
	}
	return notes
}

// subtitleTarget names a subtitle file after its video, keeping the
// language and the forced and SDH marks its name states. A name that is
// taken gets a number; it returns "" when no free name is left.
func subtitleTarget(video, rest, ext string) string {
	var lang, flags []string
	for _, tok := range strings.FieldsFunc(rest, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == ' ' }) {
		switch t := strings.ToLower(tok); {
		case t == "forced":
			flags = append(flags, "forced")
		case t == "sdh" || t == "cc":
			flags = append(flags, "sdh")
		case len(lang) == 0:
			if l, ok := release.LanguageNamed(t); ok {
				lang = append(lang, string(l))
			}
		}
	}
	base := strings.TrimSuffix(video, filepath.Ext(video))
	tags := strings.Join(append(lang, flags...), ".")
	for n := 1; n <= 9; n++ {
		name := base
		if n > 1 {
			name += fmt.Sprintf(".%d", n)
		}
		if tags != "" {
			name += "." + tags
		}
		if _, err := os.Lstat(name + ext); errors.Is(err, fs.ErrNotExist) {
			return name + ext
		}
	}
	return ""
}

// contains reports whether dir is parent or lies inside it.
func contains(parent, dir string) bool {
	return dir == parent || strings.HasPrefix(dir, parent+string(filepath.Separator))
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

var seasonFolder = regexp.MustCompile(`^Season\.\d+$`)

// ClaimFiles claims a show's files while the database still records them,
// so DeleteFiles may remove them after the records went.
func (s *Service) ClaimFiles(ctx context.Context, paths []string) ([]guard.File, error) {
	files := make([]guard.File, 0, len(paths))
	for _, p := range paths {
		f, err := s.guard.Recorded(ctx, p)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// DeleteFiles removes a show's claimed files and their subtitle files,
// then the season and show folders that are left empty. Only folders named
// the way Garner names them are removed, so a library root or someone
// else's folder never goes. It returns the files it removed and the
// problems it met.
func (s *Service) DeleteFiles(ctx context.Context, seriesID int64, files []guard.File) (removed, problems []string) {
	showSuffix := fmt.Sprintf(".[tvdbid-%d]", seriesID)
	emptied := map[string]guard.File{} // by folder: a file that lay in it
	for _, f := range files {
		if err := s.guard.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, err.Error())
			continue
		} else if err == nil {
			removed = append(removed, f.Path())
		}
		s.guard.RemoveSubtitles(f)
		emptied[filepath.Dir(f.Path())] = f
	}
	folders := s.guard.Folders(ctx)
	for _, season := range slices.Sorted(maps.Keys(emptied)) {
		if !seasonFolder.MatchString(filepath.Base(season)) {
			continue
		}
		stop := season
		if show := filepath.Dir(season); strings.HasSuffix(filepath.Base(show), showSuffix) {
			// Stays while other seasons are left; it goes with the last.
			stop = show
		}
		folders.RemoveEmpty(emptied[season], stop)
	}
	return removed, problems
}

// Removed tells Jellyfin that files are gone. When that fails, Jellyfin
// drops them at its next library scan.
func (s *Service) Removed(ctx context.Context, paths []string) {
	if s.jellyfin == nil {
		return
	}
	var remote []string
	for _, p := range paths {
		if r, ok := s.opts.JellyfinPaths.Remote(p); ok {
			remote = append(remote, r)
		}
	}
	if len(remote) == 0 {
		return
	}
	// The files are gone: tell Jellyfin even when the caller stopped waiting.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.jellyfin.Deleted(ctx, remote); err != nil {
		s.log.Warn("could not tell Jellyfin about deleted files; it drops them at its next scan", "err", err)
	}
}
