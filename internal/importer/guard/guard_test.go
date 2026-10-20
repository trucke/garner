package guard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/trucke/garner/internal/config"
)

// share is a temporary share with SABnzbd's folders, a TV library and
// folders that are neither. recorded holds the files the database records.
type share struct {
	t        *testing.T
	root     string
	recorded map[string]bool
}

func newShare(t *testing.T) *share {
	return &share{t: t, root: t.TempDir(), recorded: map[string]bool{}}
}

func (s *share) path(rel string) string { return filepath.Join(s.root, filepath.FromSlash(rel)) }

func (s *share) dir(rel string) string {
	s.t.Helper()
	if err := os.MkdirAll(s.path(rel), 0o755); err != nil {
		s.t.Fatal(err)
	}
	return s.path(rel)
}

func (s *share) file(rel string) string {
	s.t.Helper()
	s.dir(filepath.Dir(rel))
	if err := os.WriteFile(s.path(rel), []byte("x"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	return s.path(rel)
}

func (s *share) exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// guard owns library/tv, with library/anime set but missing, and SABnzbd's
// folders as paths maps them.
func (s *share) guard(paths config.PathMap, jellyfin func(context.Context) ([]string, error)) *Guard {
	return New(Options{
		Library:        config.Library{TV: s.path("library/tv"), Anime: s.path("library/anime")},
		SABnzbdPaths:   paths,
		Recorded:       func(_ context.Context, p string) (bool, error) { return s.recorded[p], nil },
		LibraryFolders: jellyfin,
	})
}

func isRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

const title = "Show.S01E01-GRP"

// TestJob lets Garner delete only a folder named after the release, inside
// the sabnzbd.paths folders when there are any and apart from the library,
// with and without paths entries.
func TestJob(t *testing.T) {
	s := newShare(t)
	for _, d := range []string{"usenet/complete/tv/" + title, "usenet/complete/tv/" + title + ".1", "usenet/complete/tv/tv", "library/tv", "other/" + title} {
		s.dir(d)
	}
	for _, setup := range []struct {
		name  string
		paths config.PathMap
		// storage is where SABnzbd reports rel under data/ in the share.
		storage func(rel string) string
	}{
		{"different paths", config.PathMap{"/data/usenet": s.path("usenet")}, func(rel string) string { return "/data/" + rel }},
		{"same paths", nil, s.path},
	} {
		g := s.guard(setup.paths, nil)
		for _, tc := range []struct {
			rel, title    string
			mapped, local bool // deletable with different paths, with the same paths
		}{
			{"usenet/complete/tv/" + title, title, true, true},
			{"usenet/complete/tv/" + title + ".1", title, true, true},
			{"usenet/complete/tv/" + title + "/" + title + ".mkv", title, true, true}, // a single-file job names the file
			{"usenet/complete/tv/" + title + ".mkv", title, false, false},             // a file directly in the category folder
			{"usenet/complete/tv/" + title, "Other.S01E01-GRP", false, false},         // named after another release
			{"usenet/complete/tv", title, false, false},                               // a category folder is not named after the release
			{"other/" + title, title, false, true},                                    // outside sabnzbd.paths, which exist only when mapped
		} {
			want := tc.mapped
			if setup.paths == nil {
				want = tc.local
			}
			j, err := g.Job(setup.storage(tc.rel), tc.title)
			if deletable := err == nil && j.KeepReason() == ""; deletable != want {
				t.Errorf("%s: Job(%s, %s) = %+v, %v; want deletable=%v", setup.name, tc.rel, tc.title, j, err, want)
			}
		}
		if j, err := g.Job("", title); err == nil {
			t.Errorf("%s: a download without storage has a job folder %+v", setup.name, j)
		}
	}

	g := s.guard(config.PathMap{"/data/usenet": s.path("usenet")}, nil)
	// The mapped folder itself is never a job folder.
	if j, err := g.Job("/data/usenet", "usenet"); err == nil {
		t.Errorf("the mapped folder is a job folder %+v", j)
	}
	// A symbolic link on the way into the library does not make it a job.
	os.Symlink(s.path("library"), s.path("usenet/linked"))
	s.dir("library/" + title)
	if j, err := g.Job("/data/usenet/linked/"+title, title); err == nil {
		t.Errorf("a folder reached through a link into the library is a job folder %+v", j)
	}
	// A library folder inside SABnzbd's job folder is never deleted with it.
	g.opts.Library.TV = s.path("usenet/complete/tv/" + title + "/library")
	if j, err := g.Job("/data/usenet/complete/tv/"+title, title); err == nil {
		t.Errorf("a folder holding the library is a job folder %+v", j)
	}
}

// TestClaims claims only the download's files, recorded files and the
// subtitles of a claimed video.
func TestClaims(t *testing.T) {
	ctx := context.Background()
	s := newShare(t)
	video := s.file("usenet/" + title + "/x7f3a.mkv")
	elsewhere := s.file("other/private.mkv")
	os.Symlink(elsewhere, s.path("usenet/"+title+"/linked.mkv"))
	g := s.guard(nil, nil)
	job, err := g.Job(s.path("usenet/"+title), title)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.InJob(job, video); err != nil {
		t.Errorf("the download's file: %v", err)
	}
	for _, p := range []string{elsewhere, s.path("usenet/" + title + "/linked.mkv")} {
		if _, err := g.InJob(job, p); !isRefusal(err) {
			t.Errorf("claimed %s as the download's file: %v", p, err)
		}
	}

	file := s.file("library/tv/Show/Season.01/Show.S01E01.mkv")
	if _, err := g.Recorded(ctx, file); !isRefusal(err) {
		t.Errorf("claimed a file the database does not record: %v", err)
	}
	s.recorded[file] = true
	claimed, err := g.Recorded(ctx, file)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.Subtitle(claimed, s.file("library/tv/Show/Season.01/Subs/Show.S01E01.de.srt")); err != nil {
		t.Errorf("the video's subtitle: %v", err)
	}
	for _, p := range []string{s.file("library/tv/Show/Season.01/tvshow.nfo"), s.file("other/Show.S01E01.de.srt")} {
		if _, err := g.Subtitle(claimed, p); !isRefusal(err) {
			t.Errorf("claimed %s as the video's subtitle: %v", p, err)
		}
	}
}

// TestWrites writes only inside an existing library root, overwrites only
// recorded files and deletes files, not folders.
func TestWrites(t *testing.T) {
	ctx := context.Background()
	s := newShare(t)
	s.dir("library/tv")
	g := s.guard(nil, nil)
	job, err := g.Job(s.dir("usenet/"+title), title)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(rel string) File {
		t.Helper()
		f, err := g.InJob(job, s.file("usenet/"+title+"/"+rel))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	if err := g.Mkdir(s.path("library/tv/Show/Season.01")); err != nil {
		t.Fatalf("a folder in the library: %v", err)
	}
	if err := g.Move(ctx, claim("a.mkv"), s.path("library/tv/Show/Season.01/a.mkv")); err != nil {
		t.Errorf("a move into the library: %v", err)
	}

	os.Symlink(s.dir("other"), s.path("library/tv/Escape"))
	if err := g.Mkdir(s.path("library/tv")); !isRefusal(err) {
		t.Errorf("Mkdir of the library root itself: %v", err)
	}
	for _, dir := range []string{
		s.path("library/anime/Show"),     // a root that is missing is not created
		s.path("other/Show"),             // outside the library
		s.path("library/tv/Escape/Show"), // through a link out of the library
	} {
		if err := g.Mkdir(dir); !isRefusal(err) {
			t.Errorf("Mkdir(%s): %v", dir, err)
		}
		if err := g.Move(ctx, claim("b.mkv"), filepath.Join(dir, "b.mkv")); !isRefusal(err) {
			t.Errorf("Move into %s: %v", dir, err)
		}
	}
	if s.exists(s.path("library/anime")) || s.exists(s.path("other/Show")) {
		t.Error("the guard created a folder it refused")
	}

	// It overwrites a recorded file, as an upgrade with the same name does,
	// and nothing else.
	foreign := s.file("library/tv/Show/Season.01/foreign.mkv")
	if err := g.Move(ctx, claim("c.mkv"), foreign); !isRefusal(err) {
		t.Errorf("overwrote a file Garner does not record: %v", err)
	}
	s.recorded[foreign] = true
	if err := g.Move(ctx, claim("c.mkv"), foreign); err != nil {
		t.Errorf("replacing a recorded file: %v", err)
	}

	s.dir("usenet/" + title + "/Sample")
	folder, err := g.InJob(job, s.path("usenet/"+title+"/Sample"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(folder); !isRefusal(err) || !s.exists(folder.Path()) {
		t.Errorf("Remove deleted a folder: %v", err)
	}
}

// TestRemoveJob deletes a job folder unless it must be kept or holds a
// recorded file.
func TestRemoveJob(t *testing.T) {
	ctx := context.Background()
	s := newShare(t)
	g := s.guard(nil, nil)
	s.file("usenet/" + title + "/x7f3a.mkv")

	if j, err := g.Job(s.path("usenet/"+title), "Other.S01E01-GRP"); err != nil || j.KeepReason() == "" {
		t.Fatalf("a folder named after another release: %+v, %v", j, err)
	} else if err := g.RemoveJob(ctx, j); !isRefusal(err) || !s.exists(s.path("usenet/"+title)) {
		t.Errorf("removed a folder named after another release: %v", err)
	}

	job, err := g.Job(s.path("usenet/"+title), title)
	if err != nil {
		t.Fatal(err)
	}
	recorded := s.file("usenet/" + title + "/Show.S01E01.mkv")
	s.recorded[recorded] = true
	if err := g.RemoveJob(ctx, job); !isRefusal(err) || !s.exists(recorded) {
		t.Errorf("removed a job folder that holds a recorded file: %v", err)
	}
	delete(s.recorded, recorded)
	if err := g.RemoveJob(ctx, job); err != nil || s.exists(s.path("usenet/"+title)) {
		t.Errorf("RemoveJob: %v", err)
	}
}

// TestRemoveEmpty removes the folders a removal emptied up to the show's
// folder, never a library root, a folder above one or one of Jellyfin's
// library folders, and outside the library only when Jellyfin's list is
// known.
func TestRemoveEmpty(t *testing.T) {
	ctx := context.Background()
	s := newShare(t)
	jellyfin := s.dir("jellyfin/tv") // a library folder of Jellyfin's outside Garner's
	listed := func(context.Context) ([]string, error) { return []string{jellyfin}, nil }
	unknown := func(context.Context) ([]string, error) { return nil, errors.New("Jellyfin did not answer") }
	removed := func(g *Guard, rel string) File {
		t.Helper()
		p := s.file(rel)
		s.recorded[p] = true
		f, err := g.Recorded(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Remove(f); err != nil {
			t.Fatal(err)
		}
		return f
	}

	g := s.guard(nil, listed)
	show := s.path("library/tv/Show.[tvdbid-1]")
	f := removed(g, "library/tv/Show.[tvdbid-1]/Season.01/a.mkv")
	if err := g.Folders(ctx).RemoveEmpty(f, show); err != nil || s.exists(show) {
		t.Errorf("the emptied show folder: %v", err)
	}

	// A library root and the folders above it stay, even when empty.
	f = removed(g, "library/tv/a.mkv")
	if err := g.Folders(ctx).RemoveEmpty(f, s.root); !isRefusal(err) || !s.exists(s.path("library/tv")) {
		t.Errorf("removed the library root: %v", err)
	}

	// Outside the library, the emptied folders go, Jellyfin's library
	// folder stays.
	f = removed(g, "jellyfin/tv/Old Show/Season 1/a.mkv")
	if err := g.Folders(ctx).RemoveEmpty(f, s.root); !isRefusal(err) || s.exists(s.path("jellyfin/tv/Old Show")) || !s.exists(jellyfin) {
		t.Errorf("Jellyfin's library folder: %v", err)
	}

	// Without Jellyfin's list, nothing goes outside the library.
	for name, list := range map[string]func(context.Context) ([]string, error){"no Jellyfin": nil, "no answer": unknown} {
		g := s.guard(nil, list)
		f := removed(g, "jellyfin/tv/Other Show/a.mkv")
		if err := g.Folders(ctx).RemoveEmpty(f, s.path("jellyfin/tv/Other Show")); !isRefusal(err) || !s.exists(s.path("jellyfin/tv/Other Show")) {
			t.Errorf("%s: removed a folder outside the library: %v", name, err)
		}
		os.Remove(s.path("jellyfin/tv/Other Show"))
	}

	// stop must hold the file.
	f = removed(g, "library/tv/Show2/a.mkv")
	if err := g.Folders(ctx).RemoveEmpty(f, s.path("library/tv/Show3")); !isRefusal(err) || !s.exists(s.path("library/tv/Show2")) {
		t.Errorf("RemoveEmpty with a stop that does not hold the file: %v", err)
	}
}
