package store

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/trucke/garner/internal/token"
)

// openAt builds a database in dataDir at a migration version, as an older
// Garner left it. Close it before Open takes the file over.
func openAt(t *testing.T, dataDir string, version int64) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, _, err := connect(ctx, dataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p, err := migrator(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, version); err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// inspect opens a copy of a database file and returns its migration version
// and how many tokens it holds.
func inspect(t *testing.T, file string) (version, tokens int64) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, _, err := connect(context.Background(), dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := migrator(db)
	if err != nil {
		t.Fatal(err)
	}
	if version, err = p.GetDBVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM api_tokens").Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	return version, tokens
}

func newestMigration(t *testing.T) int64 {
	t.Helper()
	db := openAt(t, t.TempDir(), 1)
	p, err := migrator(db)
	if err != nil {
		t.Fatal(err)
	}
	sources := p.ListSources()
	return sources[len(sources)-1].Version
}

// upgrade opens the database in dir, which an older Garner left at
// version from, and checks the backup it takes before migrating: it opens,
// holds the old version and its token, and only Garner's account can read
// it.
func upgrade(t *testing.T, dir string, from int64) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migration-v*.db"))
	if len(backups) != 1 {
		t.Fatalf("backups before migrating: %q, want one", backups)
	}
	if info, err := os.Stat(backups[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("backup %s: %v, %v; want mode 0600", backups[0], info.Mode(), err)
	}
	if version, tokens := inspect(t, backups[0]); version != from || tokens != 1 {
		t.Errorf("backup at v%d with %d tokens; want v%d with 1", version, tokens, from)
	}
	status, err := st.Status(ctx)
	if err != nil || status.LastBackup == nil || status.LastBackup.Path != backups[0] || status.LastBackup.Reason != "pre-migration" {
		t.Errorf("status = %+v, %v; want the backup before migrating", status.LastBackup, err)
	}
	return st
}

// TestUpgradeFromTheFirstMigration opens a database from Garner's first
// version: its token still works.
func TestUpgradeFromTheFirstMigration(t *testing.T) {
	dir := t.TempDir()
	db := openAt(t, dir, 1)
	plain, hash, last4 := token.New()
	exec(t, db, "INSERT INTO api_tokens (name, hash, last4, created_at) VALUES ('laptop', ?, ?, 1)", hash, last4)
	db.Close()

	st := upgrade(t, dir, 1)
	if _, err := st.Authenticate(context.Background(), plain); err != nil {
		t.Errorf("the token after upgrading: %v", err)
	}
}

// TestUpgradeFromThePreviousMigration opens a database one migration
// behind: a show, its download, the file the download brought and a token
// survive.
func TestUpgradeFromThePreviousMigration(t *testing.T) {
	ctx := context.Background()
	from := newestMigration(t) - 1
	dir := t.TempDir()
	db := openAt(t, dir, from)
	plain, hash, last4 := token.New()
	exec(t, db, "INSERT INTO api_tokens (name, hash, last4, created_at) VALUES ('laptop', ?, ?, 1)", hash, last4)
	seedShow(t, db, from)
	db.Close()

	st := upgrade(t, dir, from)
	if _, err := st.Authenticate(ctx, plain); err != nil {
		t.Errorf("the token after upgrading: %v", err)
	}
	if d, err := st.LoadSeries(ctx, 42); err != nil || d.Series.Name != "Harbor Precinct" || len(d.Episodes) != 1 {
		t.Errorf("show = %+v, %v", d.Series, err)
	}
	if d, err := st.GetDownload(ctx, 7); err != nil || d.Title != "Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP" || d.State != Completed || d.SearchSeason != 0 || d.Posting != "" {
		t.Errorf("download = %+v, %v", d, err)
	}
	if f, found, err := st.EpisodeFile(ctx, 101); err != nil || !found || f.Path != showFile {
		t.Errorf("file = %+v, %v, %v", f, found, err)
	}
}

const showFile = "/library/tv/Harbor.Precinct.[tvdbid-42]/Season.01/Harbor.Precinct.S01E01.mkv"

// seedShow adds a show with one episode, a completed download of it and
// the file the download brought, in the tables as the migration version
// left them: from 5 on, and with profiles in their own table from 19.
func seedShow(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	profile := "profile) VALUES (42, 'harbor-precinct', 'Harbor Precinct', 'eng', 'usa', 'Ended', 'official', '2026-01-01', 1, 1, 'tv')"
	if version >= 19 {
		profile = `type, profile_id) VALUES (42, 'harbor-precinct', 'Harbor Precinct', 'eng', 'usa', 'Ended', 'official', '2026-01-01', 1, 1,
			'tv', (SELECT id FROM profiles WHERE default_tv = 1))`
	}
	exec(t, db, `INSERT INTO series (id, slug, name, original_language, original_country, status, default_order, tvdb_updated, added_at, fetched_at, `+profile)
	exec(t, db, "INSERT INTO episodes (id, series_id, is_movie) VALUES (101, 42, 0)")
	exec(t, db, "INSERT INTO episode_numbers (episode_id, series_id, order_type, season, number) VALUES (101, 42, 'official', 1, 1)")
	exec(t, db, `INSERT INTO downloads (id, episode_id, title, indexer, link, size, verdict, tier, manual, state, detail, progress,
		time_left, storage, failure, retryable, stages, created_at, updated_at)
		VALUES (7, 101, 'Harbor.Precinct.S01E01.German.DL.720p.WEB-GRP', 'One', 'x', 1, 'accept', 'German 720p', 0, 'completed', '', 100,
		'', '', '', 0, '[]', 1, 1)`)
	exec(t, db, "INSERT INTO episode_files (episode_id, path, size, download_id, imported_at) VALUES (101, ?, 1, 7, 1)", showFile)
}

// TestMigrationFileTier checks migration 15: a file Garner imported takes
// the tier of the download that brought it; a file it took over has none.
func TestMigrationFileTier(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, t.TempDir(), 14)
	seedShow(t, db, 14)
	exec(t, db, "INSERT INTO episodes (id, series_id, is_movie) VALUES (102, 42, 0)")
	exec(t, db, "INSERT INTO episode_files (episode_id, path, size, imported_at) VALUES (102, '/library/tv/taken.mkv', 1, 1)")
	p, err := migrator(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	for episode, want := range map[int64]string{101: "German 720p", 102: ""} {
		var tier string
		if err := db.QueryRow("SELECT tier FROM episode_files WHERE episode_id = ?", episode).Scan(&tier); err != nil || tier != want {
			t.Errorf("episode %d: tier %q, %v; want %q", episode, tier, err, want)
		}
	}
}

// TestBackup writes a snapshot that opens and only Garner's account can
// read, and never overwrites a file.
func TestBackup(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateToken(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	b, err := st.Backup(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(b.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("backup %s: %v, %v; want mode 0600", b.Path, info.Mode(), err)
	}
	if version, tokens := inspect(t, b.Path); version != newestMigration(t) || tokens != 1 {
		t.Errorf("backup at v%d with %d tokens; want the newest with 1", version, tokens)
	}
	if status, err := st.Status(ctx); err != nil || status.LastBackup == nil || status.LastBackup.Reason != "manual" {
		t.Errorf("status = %+v, %v; want the manual backup", status.LastBackup, err)
	}
	if _, err := st.Backup(ctx, b.Path); err == nil {
		t.Error("a backup overwrote an existing file")
	}
}
