// Package guard is the only way the importer changes files and folders.
// It changes only what Garner owns, the same with and without path
// mappings in garner.toml:
//
//  1. It writes only inside library.tv and library.anime, and never
//     creates those folders themselves.
//  2. It moves or deletes a file only when the database records it as
//     Garner's, or lists it as one Garner is about to remove, or when it
//     lies where SABnzbd put the download being imported. Subtitle files
//     count with the video they belong to.
//  3. It deletes a whole folder only when it is a download's job folder
//     and holds no file the database records.
//  4. It removes a folder only when it is empty after Garner moved or
//     deleted a file in it, and never a library root, a folder above one
//     or one of Jellyfin's library folders.
//
// What Garner may move or delete is claimed first, as a Job or a File,
// while the evidence holds: a record can move on before its file goes, as
// when an upgrade replaces a file. The guard refuses everything else with
// a *Refusal that says why.
package guard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trucke/garner/internal/config"
)

// Options says what Garner owns.
type Options struct {
	// Library holds the roots Garner writes to.
	Library config.Library
	// SABnzbdPaths translates SABnzbd's folders to Garner's view.
	SABnzbdPaths config.PathMap
	// Recorded reports whether the database records the file at path as an
	// episode's file.
	Recorded func(ctx context.Context, path string) (bool, error)
	// Planned reports whether the database lists the file at path as one
	// Garner is about to remove.
	Planned func(ctx context.Context, path string) (bool, error)
	// LibraryFolders lists Jellyfin's library folders as Garner reaches
	// them; nil without Jellyfin.
	LibraryFolders func(ctx context.Context) ([]string, error)
	// Rename moves a file once the guard allowed it: os.Rename, or the
	// simulation's, which knows which folders lie on different disks.
	Rename func(oldpath, newpath string) error
	// Before, if set, runs right before each change to a file or folder the
	// guard allowed, with the change ("mkdir", "rename", "remove" or
	// "remove all") and its path. The simulation crashes there, or runs
	// another step first.
	Before func(op, path string)
}

// Guard checks every change to files and folders against what Garner owns.
type Guard struct{ opts Options }

// New returns a guard for what opts says Garner owns.
func New(opts Options) *Guard {
	if opts.Rename == nil {
		opts.Rename = os.Rename
	}
	if opts.Before == nil {
		opts.Before = func(string, string) {}
	}
	if opts.Planned == nil {
		opts.Planned = func(context.Context, string) (bool, error) { return false, nil }
	}
	return &Guard{opts: opts}
}

// Refusal is why the guard would not change a file or folder.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

var subtitleExtensions = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sup": true, ".sub": true, ".idx": true}

// IsSubtitle reports whether path is an external subtitle file Jellyfin
// reads.
func IsSubtitle(path string) bool { return subtitleExtensions[strings.ToLower(filepath.Ext(path))] }

// Job is where SABnzbd put a download's files, as Garner reaches them.
type Job struct {
	dir  string // the job folder, or the file or folder SABnzbd reported
	real string // dir with symbolic links resolved
	keep string // why the folder itself must not be deleted; empty when it may
}

// Dir is the job's folder with symbolic links resolved, or what SABnzbd
// reported when it named no job folder.
func (j Job) Dir() string { return j.real }

// KeepReason says why the job's folder must not be deleted; it is empty
// when the folder may go.
func (j Job) KeepReason() string { return j.keep }

// repeatedJob is the suffix SABnzbd adds when a job name already exists.
var repeatedJob = regexp.MustCompile(`^\.\d+$`)

// Job finds the files of the download of title that SABnzbd reports at
// storage. Garner may move and delete them when they lie inside the
// sabnzbd.paths folders, if there are any, and apart from the library.
// It may delete the folder itself only when the folder is named after the
// release (SABnzbd may append ".1" for a repeated name), which rules out
// category folders. When nothing is there any more, the error satisfies
// errors.Is(err, fs.ErrNotExist).
func (g *Guard) Job(storage, title string) (Job, error) {
	if storage == "" {
		return Job{}, refuse("SABnzbd did not say where the files are")
	}
	local, ok := g.opts.SABnzbdPaths.Local(storage)
	if !ok {
		return Job{}, refuse("no sabnzbd.paths entry covers %s", storage)
	}
	local = filepath.Clean(local)
	dir, keep := local, ""
	// For a job with a single file, SABnzbd reports the file, which lies in
	// the job folder.
	if !isJobName(filepath.Base(dir), title) && isJobName(filepath.Base(filepath.Dir(dir)), title) {
		dir = filepath.Dir(dir)
	}
	if name := filepath.Base(dir); !isJobName(name, title) {
		dir, keep = local, fmt.Sprintf("its name %q is not the release's", name)
	} else if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		dir, keep = local, dir+" is not a folder"
	}
	// Compare real paths, so a symbolic link on the way cannot lead into the
	// library or a mapped folder.
	real, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Job{}, err
	}
	if err != nil {
		return Job{}, refuse("cannot resolve %s: %v", dir, unwrapPath(err))
	}
	inside := len(g.opts.SABnzbdPaths) == 0
	for _, mapped := range g.opts.SABnzbdPaths {
		root := realPath(mapped)
		if contains(real, root) {
			return Job{}, refuse("%s is or contains the mapped folder %s", dir, root)
		}
		inside = inside || contains(root, real)
	}
	if !inside {
		return Job{}, refuse("%s is outside the sabnzbd.paths folders", dir)
	}
	for _, lib := range g.roots() {
		lib = realPath(lib)
		if contains(real, lib) || contains(lib, real) {
			return Job{}, refuse("%s overlaps the library folder %s", dir, lib)
		}
	}
	return Job{dir: dir, real: real, keep: keep}, nil
}

// isJobName reports whether a folder name is SABnzbd's job name for the
// release: its title, or its title with ".1" and so on for a repeat.
func isJobName(name, title string) bool {
	suffix, found := cutPrefixFold(name, title)
	return found && (suffix == "" || repeatedJob.MatchString(suffix))
}

// File is a file Garner may move or delete.
type File struct{ path string }

// Path is where the file was when it was claimed.
func (f File) Path() string { return f.path }

// InJob claims a file where SABnzbd put the download.
func (g *Guard) InJob(j Job, path string) (File, error) {
	path = filepath.Clean(path)
	if j.real == "" || !contains(j.real, realPath(path)) {
		return File{}, refuse("%s is not among the download's files", path)
	}
	return File{path: path}, nil
}

// Recorded claims a file the database records as an episode's file,
// imported or taken over. The claim holds after the record moves on.
func (g *Guard) Recorded(ctx context.Context, path string) (File, error) {
	ok, err := g.opts.Recorded(ctx, path)
	if err != nil {
		return File{}, err
	}
	if !ok {
		return File{}, refuse("Garner does not record %s as an episode's file", path)
	}
	return File{path: filepath.Clean(path)}, nil
}

// Planned claims a file the database lists as one Garner is about to
// remove, such as the version an upgrade replaced when a crash came after
// the new record and before the removal.
func (g *Guard) Planned(ctx context.Context, path string) (File, error) {
	ok, err := g.opts.Planned(ctx, path)
	if err != nil {
		return File{}, err
	}
	if !ok {
		return File{}, refuse("Garner did not plan to remove %s", path)
	}
	return File{path: filepath.Clean(path)}, nil
}

// Subtitle claims a subtitle file of a claimed video: one in the video's
// folder or below it.
func (g *Guard) Subtitle(video File, path string) (File, error) {
	path = filepath.Clean(path)
	if !IsSubtitle(path) {
		return File{}, refuse("%s is not a subtitle file", path)
	}
	if video.path == "" || !contains(filepath.Dir(video.path), path) {
		return File{}, refuse("%s does not lie with %s", path, video.path)
	}
	return File{path: path}, nil
}

// Mkdir creates dir and the folders above it inside a library root, which
// must exist: Garner never creates the library folders themselves.
func (g *Guard) Mkdir(dir string) error {
	if err := g.inLibrary(dir); err != nil {
		return err
	}
	dir = filepath.Clean(dir)
	var root string
	for _, r := range g.roots() {
		if contains(r, dir) {
			root = r
		}
	}
	g.opts.Before("mkdir", dir)
	// One folder at a time below the root: os.Mkdir fails when the folder
	// above is missing, so a root that went since the check, as when its
	// share was unmounted, is not created again for files to go into.
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	path := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		err := os.Mkdir(path, 0o775)
		if errors.Is(err, fs.ErrExist) {
			st, serr := os.Stat(path)
			if serr != nil {
				return serr
			}
			if !st.IsDir() {
				return fmt.Errorf("%s is not a folder", path)
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Move renames f to target inside a library root. It overwrites only a
// file the database records, as when an upgrade keeps the name of the file
// it replaces. The folder for target must exist; see Mkdir.
func (g *Guard) Move(ctx context.Context, f File, target string) error {
	if err := g.inLibrary(target); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		ours, err := g.opts.Recorded(ctx, target)
		if err != nil {
			return err
		}
		if !ours {
			return refuse("%s exists already and is not Garner's", target)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	g.opts.Before("rename", f.path)
	return g.opts.Rename(f.path, target)
}

// Remove deletes the file f, never a folder.
func (g *Guard) Remove(f File) error {
	st, err := os.Lstat(f.path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return refuse("%s is a folder, not a file", f.path)
	}
	g.opts.Before("remove", f.path)
	return os.Remove(f.path)
}

// RemoveSubtitles deletes the external subtitle files of video that are
// named after it in its folder, such as those of a version an upgrade
// replaced.
func (g *Guard) RemoveSubtitles(video File) {
	base := strings.TrimSuffix(filepath.Base(video.path), filepath.Ext(video.path)) + "."
	entries, err := os.ReadDir(filepath.Dir(video.path))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), base) && IsSubtitle(e.Name()) {
			p := filepath.Join(filepath.Dir(video.path), e.Name())
			g.opts.Before("remove", p)
			os.Remove(p)
		}
	}
}

// RemoveJob deletes a download's job folder with everything in it. It
// refuses a folder that must be kept (see Job) and one that holds a file
// the database records.
func (g *Guard) RemoveJob(ctx context.Context, j Job) error {
	if j.real == "" {
		return refuse("no job folder")
	}
	if j.keep != "" {
		return &Refusal{Reason: j.keep}
	}
	var recorded string
	err := filepath.WalkDir(j.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		ours, err := g.opts.Recorded(ctx, p)
		if err != nil {
			return err
		}
		if ours {
			recorded = p
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if recorded != "" {
		return refuse("it holds %s, which Garner records as an episode's file", recorded)
	}
	g.opts.Before("remove all", j.real)
	return os.RemoveAll(j.real)
}

// Folders removes the folders that moves and deletions leave empty during
// one operation. It asks Jellyfin for its library folders once.
type Folders struct {
	guard    *Guard
	ctx      context.Context
	asked    bool
	known    bool // Jellyfin's list is known
	jellyfin []string
}

// Folders starts removing empty folders for one operation.
func (g *Guard) Folders(ctx context.Context) *Folders { return &Folders{guard: g, ctx: ctx} }

// RemoveEmpty removes the folder that emptied lay in and the folders above
// it while they are empty, up to and including stop, which must hold
// emptied. It never removes a library root or a folder above one, nor one
// of Jellyfin's library folders or a folder above one; without Jellyfin's
// list, it removes only folders inside a library root. It returns the
// refusal that stopped it, if one did.
func (f *Folders) RemoveEmpty(emptied File, stop string) error {
	stop = filepath.Clean(stop)
	if emptied.path == "" || emptied.path == stop || !contains(stop, emptied.path) {
		return refuse("%s does not hold %s", stop, emptied.path)
	}
	for dir := filepath.Dir(emptied.path); contains(stop, dir); dir = filepath.Dir(dir) {
		if err := f.protected(dir); err != nil {
			return err
		}
		f.guard.opts.Before("remove", dir)
		if os.Remove(dir) != nil {
			return nil // not empty, or gone already
		}
	}
	return nil
}

func (f *Folders) protected(dir string) error {
	real := realPath(dir)
	inside := false
	for _, root := range f.guard.roots() {
		root = realPath(root)
		if contains(real, root) {
			return refuse("%s is a library folder or holds one", dir)
		}
		inside = inside || contains(root, real)
	}
	if !f.asked {
		f.asked = true
		if list := f.guard.opts.LibraryFolders; list != nil {
			folders, err := list(f.ctx)
			f.jellyfin, f.known = folders, err == nil
		}
	}
	if !f.known && !inside {
		return refuse("%s lies outside the library folders, and Jellyfin's library folders are unknown", dir)
	}
	for _, lib := range f.jellyfin {
		if contains(real, realPath(lib)) {
			return refuse("%s is one of Jellyfin's library folders or holds one", dir)
		}
	}
	return nil
}

// inLibrary checks that path lies inside an existing library root, also
// after resolving symbolic links.
func (g *Guard) inLibrary(path string) error {
	path = filepath.Clean(path)
	for _, root := range g.roots() {
		if path == root {
			return refuse("%s is a library folder itself", path)
		}
		if !contains(root, path) {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return refuse("the library folder %s is missing", root)
		}
		if p := realPath(path); p == real || !contains(real, p) {
			return refuse("%s leads out of the library folder %s", path, root)
		}
		return nil
	}
	return refuse("%s is outside the library folders", path)
}

// roots are the library folders that are set.
func (g *Guard) roots() []string {
	var roots []string
	for _, r := range []string{g.opts.Library.TV, g.opts.Library.Anime} {
		if r != "" {
			roots = append(roots, filepath.Clean(r))
		}
	}
	return roots
}

// contains reports whether dir is parent or lies inside it.
func contains(parent, dir string) bool {
	return dir == parent || strings.HasPrefix(dir, parent+string(filepath.Separator))
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

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// unwrapPath drops the path from an *fs.PathError; the message names it.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
