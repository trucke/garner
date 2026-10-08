package importer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trucke/garner/internal/release"
	"github.com/trucke/garner/internal/store"
)

// subtitleExtensions are external subtitle files Jellyfin reads.
var subtitleExtensions = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sup": true, ".sub": true, ".idx": true}

// repeatedJob is the suffix SABnzbd adds when a job name already exists.
var repeatedJob = regexp.MustCompile(`^\.\d+$`)

// tidy runs after the videos were moved: it puts the release's subtitle
// files next to their videos and removes the rest of SABnzbd's job folder.
// It never fails the import; what stays behind is returned as notes.
func (s *Service) tidy(d *store.Download, files []store.ImportFile) []string {
	dir, why := s.jobFolder(d)
	if why != "" {
		return []string{"SABnzbd's folder was left in place: " + why}
	}
	notes := s.moveSubtitles(dir, files)
	remaining, err := videoFiles(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notes
	case err != nil:
		return append(notes, fmt.Sprintf("SABnzbd's folder was left in place: %v", unwrapPath(err)))
	case len(remaining) > 0:
		return append(notes, fmt.Sprintf("SABnzbd's folder %s was left in place: it holds %s that could not be assigned to an episode",
			dir, count(len(remaining), "video file")))
	}
	if subs := subtitleFiles(dir); subs > 0 {
		return append(notes, fmt.Sprintf("SABnzbd's folder %s was left in place: it holds %s that could not be moved", dir, count(subs, "subtitle file")))
	}
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("could not remove SABnzbd's folder", "download", d.ID, "dir", dir, "err", err)
		return append(notes, fmt.Sprintf("SABnzbd's folder %s could not be removed: %v", dir, unwrapPath(err)))
	}
	s.log.Info("removed SABnzbd's folder", "download", d.ID, "dir", dir)
	return notes
}

// discard removes the job folder of a rejected release.
func (s *Service) discard(d *store.Download) {
	dir, why := s.jobFolder(d)
	if why != "" {
		s.log.Warn("left the rejected release's files in place", "download", d.ID, "reason", why)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("could not remove the rejected release's files", "download", d.ID, "dir", dir, "err", err)
		return
	}
	s.log.Info("removed the rejected release's files", "download", d.ID, "dir", dir)
}

// jobFolder is SABnzbd's folder for the download, as Garner reaches it, and
// why it must not be deleted, if so. A folder qualifies only when it is
// named after the release (SABnzbd may append ".1" for a repeated name),
// which rules out category folders, lies inside a sabnzbd.paths folder and
// neither is nor contains a mapped or library folder.
func (s *Service) jobFolder(d *store.Download) (dir, why string) {
	if d.Storage == "" {
		return "", "SABnzbd did not say where the files are"
	}
	dir, ok := s.opts.SABnzbdPaths.Local(d.Storage)
	if !ok {
		return "", "no sabnzbd.paths entry covers " + d.Storage
	}
	dir = filepath.Clean(dir)
	// For a job with a single file, SABnzbd reports the file, which lies in
	// the job folder.
	if !isJobName(filepath.Base(dir), d.Title) && isJobName(filepath.Base(filepath.Dir(dir)), d.Title) {
		dir = filepath.Dir(dir)
	}
	if name := filepath.Base(dir); !isJobName(name, d.Title) {
		return "", fmt.Sprintf("its name %q is not the release's", name)
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return "", dir + " is not a folder"
	}
	// Compare real paths, so a symbolic link on the way cannot lead into the
	// library or a mapped folder.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Sprintf("cannot resolve %s: %v", dir, unwrapPath(err))
	}
	inside := len(s.opts.SABnzbdPaths) == 0
	for _, local := range s.opts.SABnzbdPaths {
		root := realPath(local)
		if contains(real, root) {
			return "", fmt.Sprintf("%s is or contains the mapped folder %s", dir, root)
		}
		inside = inside || contains(root, real)
	}
	if !inside {
		return "", dir + " is outside the sabnzbd.paths folders"
	}
	for _, lib := range []string{s.opts.Library.TV, s.opts.Library.Anime} {
		if lib == "" {
			continue
		}
		lib = realPath(lib)
		if contains(real, lib) || contains(lib, real) {
			return "", fmt.Sprintf("%s overlaps the library folder %s", dir, lib)
		}
	}
	return real, ""
}

// isJobName reports whether a folder name is SABnzbd's job name for the
// release: its title, or its title with ".1" and so on for a repeat.
func isJobName(name, title string) bool {
	suffix, found := cutPrefixFold(name, title)
	return found && (suffix == "" || repeatedJob.MatchString(suffix))
}

// realPath resolves symbolic links in the part of p that exists, such as
// a parent of a library folder that is not there yet.
func realPath(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for dir := p; ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return p
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
	}
}

// subtitleFiles counts the subtitle files left in dir.
func subtitleFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && subtitleExtensions[strings.ToLower(filepath.Ext(p))] {
			n++
		}
		return nil
	})
	return n
}

// moveSubtitles moves the subtitle files in dir next to the videos they
// belong to, named the way Jellyfin reads external subtitles:
// "<video name>.<language>[.forced][.sdh].<ext>". In a folder with one video,
// every subtitle file belongs to it; otherwise its name must start with the
// video's original name.
func (s *Service) moveSubtitles(dir string, files []store.ImportFile) []string {
	var notes []string
	moved := 0
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !subtitleExtensions[strings.ToLower(filepath.Ext(p))] {
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
		if owner == nil && len(files) == 1 {
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
		if err := os.Rename(p, target); err != nil {
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

// removeSubtitles deletes the external subtitles of a video, such as the
// previous version's when an upgrade replaced it.
func removeSubtitles(video string) {
	base := filepath.Base(strings.TrimSuffix(video, filepath.Ext(video))) + "."
	entries, err := os.ReadDir(filepath.Dir(video))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), base) && subtitleExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			os.Remove(filepath.Join(filepath.Dir(video), e.Name()))
		}
	}
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
