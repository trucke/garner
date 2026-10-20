package importer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/inspect"
	"github.com/trucke/garner/internal/match"
	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/selection"
	"github.com/trucke/garner/internal/store"
)

// TakeOver looks for a show's video files in the library and records those
// that clearly hold its episodes, as if Garner had imported them: one file
// per episode, the better one when several hold the same episode. It looks
// in Garner's folder for the show and in the folders Jellyfin holds the
// show in, checks each file with FFprobe for its tier, so upgrades work for
// it, and leaves the files where they are. Episodes with a file keep it.
func (s *Service) TakeOver(ctx context.Context, seriesID int64) (api.TakeOver, error) {
	s.files.Lock()
	defer s.files.Unlock()
	out := api.TakeOver{Folders: []string{}, Skipped: []api.SkippedFile{}, Notes: []string{}}
	c, err := s.seriesContext(ctx, seriesID)
	if err != nil {
		return out, err
	}
	out.Folders, out.Notes = s.showFolders(ctx, c)
	have, err := s.store.SeriesFileTiers(ctx, seriesID)
	if err != nil {
		return out, err
	}
	skip := func(path, reason string) {
		out.Skipped = append(out.Skipped, api.SkippedFile{Path: path, Reason: reason})
	}

	type candidate struct {
		path     string
		size     int64
		episodes []int64
		tier     string
	}
	var found []candidate
	seen := map[string]bool{}
	for _, dir := range out.Folders {
		files, err := libraryVideos(dir)
		if err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("could not read %s: %v", dir, unwrapPath(err)))
			continue
		}
		for _, f := range files {
			if seen[f.Source] {
				continue
			}
			seen[f.Source] = true
			if used, err := s.store.FileInUse(ctx, f.Source); err != nil {
				return out, err
			} else if used {
				continue
			}
			episodes, why := c.fileEpisodes(f.Source)
			if why != "" {
				skip(f.Source, why)
				continue
			}
			p, err := s.probe.Probe(ctx, f.Source)
			switch {
			case errors.Is(err, inspect.ErrUnreadable):
				skip(f.Source, err.Error())
				continue
			case err != nil:
				return out, err
			case len(p.Video) == 0:
				skip(f.Source, "it holds no video")
				continue
			}
			found = append(found, candidate{f.Source, f.Size, episodes, c.fileTier(f.Source, p)})
		}
	}

	// The better file of an episode wins: the better tier, then the larger.
	slices.SortStableFunc(found, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(selection.TierRank(c.profile, a.tier), selection.TierRank(c.profile, b.tier)), -cmp.Compare(a.size, b.size))
	})
	taken := map[int64]bool{}
	for id := range have {
		taken[id] = true
	}
	now := s.now()
	for _, f := range found {
		if i := slices.IndexFunc(f.episodes, func(id int64) bool { return taken[id] }); i >= 0 {
			why := "a better file holds " + c.show.Label(f.episodes[i])
			if _, ok := have[f.episodes[i]]; ok {
				why = c.show.Label(f.episodes[i]) + " has a file in the library already"
			}
			skip(f.path, why)
			continue
		}
		for _, id := range f.episodes {
			taken[id] = true
			if _, err := s.store.TakeOverFile(ctx, store.EpisodeFile{EpisodeID: id, Path: f.path, Size: f.size, Tier: f.tier}, now); err != nil {
				return out, err
			}
		}
		out.Files++
		out.Episodes += len(f.episodes)
	}
	if out.Renamable, err = s.renamable(ctx, c); err != nil {
		return out, err
	}
	s.log.Info("took over files from the library", "series", seriesID, "files", out.Files, "episodes", out.Episodes,
		"skipped", len(out.Skipped), "folders", len(out.Folders))
	return out, nil
}

// showFolders lists the folders that hold a show's files, as Garner
// reaches them: its own folder for the show and those Jellyfin holds the
// show in, when they exist. notes say what could not be looked at.
func (s *Service) showFolders(ctx context.Context, c showContext) (folders, notes []string) {
	notes = []string{}
	var candidates []string
	if own, err := ShowFolder(s.opts.Library, c.data.Series, c.profile); err == nil {
		candidates = append(candidates, own)
	} else {
		notes = append(notes, err.Error())
	}
	if s.jellyfin != nil {
		remote, err := s.jellyfin.ShowFolders(ctx, c.data.Series.ID)
		if err != nil {
			notes = append(notes, "Jellyfin did not answer, so Garner looked only in its own folder: "+err.Error())
		}
		for _, r := range remote {
			local, ok := s.opts.JellyfinPaths.Local(r)
			if !ok {
				notes = append(notes, fmt.Sprintf("Jellyfin holds the show in %s, which no jellyfin.paths entry covers", r))
				continue
			}
			candidates = append(candidates, local)
		}
	}
	folders = []string{}
	for _, dir := range candidates {
		dir = filepath.Clean(dir)
		if st, err := os.Stat(dir); err == nil && st.IsDir() && !slices.Contains(folders, dir) {
			folders = append(folders, dir)
		}
	}
	return folders, notes
}

// libraryVideos lists the video files in a library folder and below it,
// without samples.
func libraryVideos(dir string) ([]store.ImportFile, error) {
	var files []store.ImportFile
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if !videoExtensions[strings.ToLower(filepath.Ext(p))] || sampleName.MatchString(rel) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		files = append(files, store.ImportFile{Source: p, Size: info.Size()})
		return nil
	})
	return files, err
}

// garnerNumber is the episode part of a name Garner gave a file, after the
// show's name: "S01E01." or "S01E01-E02.", or the end of the name.
var garnerNumber = regexp.MustCompile(`^S(\d{2,})E(\d{2,})(?:-E(\d{2,}))?(?:\.|$)`)

// fileEpisodes reads which of the show's episodes a library file holds
// from its name, or why it cannot tell.
func (c showContext) fileEpisodes(path string) ([]int64, string) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	// A name Garner gave is read directly: the episode title that follows
	// the number, such as "1.23.45", must not count as numbers.
	if rest, ok := strings.CutPrefix(name, showName(c.data.Series)+"."); ok {
		if ids := c.garnerEpisodes(rest); len(ids) > 0 {
			return ids, ""
		}
	}
	claims := release.Parse(name)
	if len(claims.Series) == 0 {
		// Names in a show's folder often leave the show out, as in
		// "S01E01 - Pilot".
		claims = release.Parse(showName(c.data.Series) + "." + name)
	}
	switch m := c.show.Match(claims, time.Time{}); m.Kind {
	case match.Episodes:
		return m.Episodes, ""
	case match.Season:
		return nil, "the name names a whole season, not episodes"
	default:
		return nil, m.Reason
	}
}

// garnerEpisodes are the episodes the number at the start of rest names in
// the aired order, such as "S01E01-E02.Title"; none when it names none or
// one of them is missing.
func (c showContext) garnerEpisodes(rest string) []int64 {
	m := garnerNumber.FindStringSubmatch(rest)
	if m == nil {
		return nil
	}
	season, _ := strconv.Atoi(m[1])
	first, _ := strconv.Atoi(m[2])
	last := first
	if m[3] != "" {
		last, _ = strconv.Atoi(m[3])
	}
	var ids []int64
	for n := first; n <= last; n++ {
		i := slices.IndexFunc(c.data.Episodes, func(e store.Episode) bool {
			p, ok := e.Numbers["official"]
			return ok && p.Season == season && p.Number == n
		})
		if i < 0 {
			return nil
		}
		ids = append(ids, c.data.Episodes[i].ID)
	}
	return ids
}

// fileTier is the tier of a library file: its tagged audio languages, else
// those its name states, else the show's original language when the
// profile wants it, with the resolution of its video. Without that, the
// tier stays unknown, so an untagged file in another language, such as a
// dub, is not taken for one the profile rejects and replaced.
func (c showContext) fileTier(path string, p inspect.Probe) string {
	var audio []release.Language
	add := func(l release.Language) {
		if l != "" && !slices.Contains(audio, l) {
			audio = append(audio, l)
		}
	}
	for _, t := range p.Audio {
		if l, ok := release.LanguageNamed(t.Language); ok {
			add(l)
		}
	}
	original, _ := release.LanguageNamed(c.data.Series.OriginalLanguage)
	if len(audio) == 0 {
		for _, a := range release.Parse(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))).Audio {
			if a.Value == release.Original {
				add(original)
			} else {
				add(a.Value)
			}
		}
	}
	if len(audio) == 0 && c.profile.Has(original) {
		add(original)
	}
	res := release.Resolution(api.ResolutionClass(p.Video[0].Width, p.Video[0].Height))
	return selection.FileTier(c.profile, audio, res)
}

// renamable counts the show's taken-over files outside Garner's folder for
// it.
func (s *Service) renamable(ctx context.Context, c showContext) (int, error) {
	own, err := ShowFolder(s.opts.Library, c.data.Series, c.profile)
	if err != nil {
		return 0, nil
	}
	files, err := s.store.TakenFiles(ctx, c.data.Series.ID)
	if err != nil {
		return 0, err
	}
	paths := map[string]bool{}
	for _, f := range files {
		if !contains(own, f.Path) {
			paths[f.Path] = true
		}
	}
	return len(paths), nil
}

// Rename moves the show's taken-over files that lie outside Garner's folder
// for it into that folder, named the way Garner names the files it imports,
// with their subtitle files. It never overwrites a file, tells Jellyfin
// about the moves and removes the folders the moves left empty. What stays
// behind is in the notes.
func (s *Service) Rename(ctx context.Context, seriesID int64) (api.Renamed, error) {
	s.files.Lock()
	defer s.files.Unlock()
	out := api.Renamed{Notes: []string{}}
	c, err := s.seriesContext(ctx, seriesID)
	if err != nil {
		return out, err
	}
	own, err := ShowFolder(s.opts.Library, c.data.Series, c.profile)
	if err != nil {
		return out, err
	}
	taken, err := s.store.TakenFiles(ctx, seriesID)
	if err != nil {
		return out, err
	}
	// One entry per file, with the episodes it holds in the aired order.
	var files []store.ImportFile
	tiers := map[string]string{}
	for _, f := range taken {
		if contains(own, f.Path) {
			continue
		}
		i := slices.IndexFunc(files, func(g store.ImportFile) bool { return g.Source == f.Path })
		if i < 0 {
			files = append(files, store.ImportFile{Source: f.Path, Size: f.Size})
			i = len(files) - 1
		}
		files[i].Episodes = append(files[i].Episodes, f.EpisodeID)
		tiers[f.Path] = f.Tier
	}
	note := func(format string, args ...any) { out.Notes = append(out.Notes, fmt.Sprintf(format, args...)) }

	var moved []store.ImportFile
	for _, f := range files {
		slices.SortFunc(f.Episodes, func(a, b int64) int {
			x, y := c.episodes[a].Numbers["official"], c.episodes[b].Numbers["official"]
			return cmp.Or(cmp.Compare(x.Season, y.Season), cmp.Compare(x.Number, y.Number))
		})
		p, err := s.probe.Probe(ctx, f.Source)
		if err != nil {
			note("%s stayed: FFprobe could not read it: %v", f.Source, unwrapPath(err))
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f.Source), filepath.Ext(f.Source))
		if f.Target, err = c.target(s.opts.Library, f.Episodes, releaseTags(name, tiers[f.Source], p), filepath.Ext(f.Source)); err != nil {
			return out, err
		}
		if _, err := os.Lstat(f.Target); err == nil {
			note("%s stayed: %s exists already", f.Source, f.Target)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.Target), 0o775); err != nil {
			note("%s stayed: cannot create %s: %v", f.Source, filepath.Dir(f.Target), unwrapPath(err))
			continue
		}
		if err := os.Rename(f.Source, f.Target); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				note("%s stayed: it is on another filesystem than the library, and Garner moves files without copying", f.Source)
			} else {
				note("%s stayed: %v", f.Source, unwrapPath(err))
			}
			continue
		}
		if err := s.store.MoveEpisodeFile(ctx, f.Source, f.Target); err != nil {
			// The file moved; the record must follow, or Garner loses it.
			return out, fmt.Errorf("record the move of %s to %s: %w", f.Source, f.Target, err)
		}
		s.log.Info("renamed a taken-over file", "series", seriesID, "from", f.Source, "to", f.Target)
		moved = append(moved, f)
	}
	out.Files = len(moved)

	// Subtitles follow their videos, folder by folder; then the emptied
	// folders go, up to the show's folder.
	byDir := map[string][]store.ImportFile{}
	for _, f := range moved {
		byDir[filepath.Dir(f.Source)] = append(byDir[filepath.Dir(f.Source)], f)
	}
	folders, _ := s.showFolders(ctx, c)
	for dir, inDir := range byDir {
		for _, n := range s.moveSubtitles(dir, inDir, true) {
			if !strings.HasPrefix(n, "moved ") {
				out.Notes = append(out.Notes, n)
			}
		}
		removeEmpty(dir, folders)
	}
	for _, dir := range folders {
		if dir == own {
			continue
		}
		if n := countFiles(dir); n > 0 && slices.ContainsFunc(moved, func(f store.ImportFile) bool { return contains(dir, f.Source) }) {
			note("%s stays with %s that Garner does not manage, so Jellyfin may still list it", dir, count(n, "other file"))
		}
	}
	s.announceMoves(ctx, moved)
	return out, nil
}

// removeEmpty removes dir and its parents while they are empty, up to and
// including the show folder among folders that holds it.
func removeEmpty(dir string, folders []string) {
	i := slices.IndexFunc(folders, func(f string) bool { return contains(f, dir) })
	if i < 0 {
		return
	}
	for ; contains(folders[i], dir); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}

// countFiles counts the files in dir and below it.
func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// announceMoves tells Jellyfin that the files left their old paths and
// appeared at the new ones. When that fails, its next scan finds them.
func (s *Service) announceMoves(ctx context.Context, moved []store.ImportFile) {
	if s.jellyfin == nil || len(moved) == 0 {
		return
	}
	var from, to []string
	for _, f := range moved {
		if r, ok := s.opts.JellyfinPaths.Remote(f.Source); ok {
			from = append(from, r)
		}
		if r, ok := s.opts.JellyfinPaths.Remote(f.Target); ok {
			to = append(to, r)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if len(from) > 0 {
		if err := s.jellyfin.Deleted(ctx, from); err != nil {
			s.log.Warn("could not tell Jellyfin about moved files; it finds them at its next scan", "err", err)
		}
	}
	if len(to) > 0 {
		if err := s.jellyfin.Created(ctx, to); err != nil {
			s.log.Warn("could not tell Jellyfin about moved files; it finds them at its next scan", "err", err)
		}
	}
}
