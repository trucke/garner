// Package store owns Garner's database: opening it, applying migrations,
// backups and the queries the server needs.
package store

//go:generate sqlc generate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"github.com/pressly/goose/v3"
	turso "turso.tech/database/tursogo"

	"github.com/trucke/garner/internal/store/query"
	"github.com/trucke/garner/internal/token"
)

//go:embed migrations/*.sql
var migrations embed.FS

// FileName is the database file inside the data directory.
const FileName = "garner.db"

var (
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("already exists")
	ErrUnauthorized = errors.New("unauthorized")
)

// Store is an open database. Garner uses a single connection: its load is
// small, and every Turso connection opens its own database instance.
type Store struct {
	db   *sql.DB
	q    *query.Queries
	path string
	log  *slog.Logger
}

// Open opens the database in dataDir, creating it if needed, and applies
// pending migrations. An existing database is backed up before migrating.
// Turso locks the file, so a second process cannot open it.
func Open(ctx context.Context, dataDir string, log *slog.Logger) (*Store, error) {
	db, path, err := connect(ctx, dataDir, log)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, q: query.New(db), path: path, log: log}
	if err := s.migrate(ctx, dataDir); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// connect opens the database in dataDir, creating it if needed, without
// migrating it.
func connect(ctx context.Context, dataDir string, log *slog.Logger) (*sql.DB, string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create data directory: %w", err)
	}
	// Turso unpacks its engine library on first use. It goes into the data
	// directory, a folder per Turso version, and once the database is open
	// the folders of other versions go. A cache directory set from outside
	// is left alone.
	libs, version := "", tursoVersion()
	if os.Getenv("TURSO_GO_CACHE_DIR") == "" {
		libs = filepath.Join(dataDir, "lib")
		os.Setenv("TURSO_GO_CACHE_DIR", filepath.Join(libs, version))
	}
	path := filepath.Join(dataDir, FileName)
	inner, err := turso.NewConnector(path)
	if err != nil {
		return nil, "", err
	}
	db := sql.OpenDB(pragmaConnector{inner})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		if strings.Contains(err.Error(), "locked by another process") {
			return nil, "", fmt.Errorf("database %s is in use by another Garner process", path)
		}
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	if libs != "" && version != "" {
		removeOtherLibraries(libs, version, log)
	}
	return db, path, nil
}

// tursoVersion is the version of the Turso library package this program
// was built with; empty when the build does not say.
func tursoVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, m := range info.Deps {
		if m.Path != "github.com/tursodatabase/turso-go-platform-libs" {
			continue
		}
		if m.Replace != nil {
			m = m.Replace
		}
		return m.Version
	}
	return ""
}

// removeOtherLibraries removes everything in libs but the folder of the
// Turso version in use: libraries of earlier versions, unpacked before an
// upgrade. Only the process holding the database lock calls it.
func removeOtherLibraries(libs, keep string, log *slog.Logger) {
	entries, err := os.ReadDir(libs)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(libs, e.Name())); err != nil {
			log.Warn("could not remove an earlier database library", "folder", e.Name(), "err", err)
			continue
		}
		log.Info("removed an earlier database library", "folder", e.Name())
	}
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file.
func (s *Store) Path() string { return s.path }

// pragmaConnector enables foreign keys on every new connection.
type pragmaConnector struct{ inner driver.Connector }

func (c pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("database driver cannot execute statements")
	}
	if _, err := exec.ExecContext(ctx, "PRAGMA foreign_keys = ON", nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	return conn, nil
}

func (c pragmaConnector) Driver() driver.Driver { return c.inner.Driver() }

// migrator returns goose's view of Garner's migrations on db.
func migrator(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return p, nil
}

func (s *Store) migrate(ctx context.Context, dataDir string) error {
	p, err := migrator(s.db)
	if err != nil {
		return err
	}
	pending, err := p.HasPending(ctx)
	if err != nil || !pending {
		return err
	}
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	var before *Backup
	if current > 0 {
		name := fmt.Sprintf("pre-migration-v%d-%s.db", current, time.Now().UTC().Format("20060102-150405"))
		b, err := s.snapshot(ctx, filepath.Join(dataDir, "backups", name))
		if err != nil {
			return fmt.Errorf("back up before migrating: %w", err)
		}
		before = &b
		s.log.Info("backed up database before migrating", "path", b.Path)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	for _, r := range results {
		s.log.Info("applied migration", "version", r.Source.Version, "duration", r.Duration.Round(time.Millisecond))
	}
	if before != nil {
		s.record(ctx, *before, "pre-migration")
	}
	return nil
}

// Token is an authenticated client.
type Token struct {
	ID   int64
	Name string
	hash string
}

// TokenInfo describes a token without revealing it.
type TokenInfo struct {
	Name     string
	Last4    string
	Created  time.Time
	LastUsed time.Time // zero if never used
}

// CreateToken creates a token named name and returns it in plain text. Only
// its hash is stored, so this is the only time it can be shown.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	plain, hash, last4 := token.New()
	_, err := s.q.CreateToken(ctx, query.CreateTokenParams{Name: name, Hash: hash, Last4: last4, CreatedAt: time.Now().Unix()})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", fmt.Errorf("token %q: %w", name, ErrExists)
		}
		return "", err
	}
	return plain, nil
}

// Authenticate returns the token matching plain, or ErrUnauthorized.
func (s *Store) Authenticate(ctx context.Context, plain string) (Token, error) {
	if !token.WellFormed(plain) {
		return Token{}, ErrUnauthorized
	}
	row, err := s.q.TokenByHash(ctx, token.Hash(plain))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrUnauthorized
	}
	if err != nil {
		return Token{}, err
	}
	// Record use at most once a minute to avoid a write per request.
	now := time.Now().Unix()
	if !row.LastUsedAt.Valid || now-row.LastUsedAt.Int64 >= 60 {
		if err := s.q.TouchToken(ctx, query.TouchTokenParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: row.ID}); err != nil {
			s.log.Warn("could not record token use", "token", row.Name, "err", err)
		}
	}
	return Token{ID: row.ID, Name: row.Name, hash: token.Hash(plain)}, nil
}

// StillValid reports whether t has not been revoked since it authenticated.
func (s *Store) StillValid(ctx context.Context, t Token) (bool, error) {
	n, err := s.q.TokenStillValid(ctx, query.TokenStillValidParams{ID: t.ID, Hash: t.hash})
	return n == 1, err
}

// Tokens lists all tokens.
func (s *Store) Tokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.q.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TokenInfo, len(rows))
	for i, r := range rows {
		out[i] = TokenInfo{Name: r.Name, Last4: r.Last4, Created: time.Unix(r.CreatedAt, 0)}
		if r.LastUsedAt.Valid {
			out[i].LastUsed = time.Unix(r.LastUsedAt.Int64, 0)
		}
	}
	return out, nil
}

// RevokeToken deletes the token named name and returns its ID.
func (s *Store) RevokeToken(ctx context.Context, name string) (int64, error) {
	id, err := s.q.DeleteToken(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("token %q: %w", name, ErrNotFound)
	}
	return id, err
}

func validName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("token name must be 1 to 64 characters")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.@", r) {
			return fmt.Errorf("token name may contain letters, digits and - _ . @, not %q", r)
		}
	}
	return nil
}

// Backup is a database snapshot.
type Backup struct {
	Path    string
	Size    int64
	Reason  string
	Created time.Time
}

// Backup writes a consistent snapshot to path, or to the data directory's
// backups folder when path is empty.
func (s *Store) Backup(ctx context.Context, path string) (Backup, error) {
	if path == "" {
		name := "garner-" + time.Now().UTC().Format("20060102-150405") + ".db"
		path = filepath.Join(filepath.Dir(s.path), "backups", name)
	}
	b, err := s.snapshot(ctx, path)
	if err != nil {
		return Backup{}, err
	}
	b.Reason = "manual"
	s.record(ctx, b, b.Reason)
	return b, nil
}

func (s *Store) snapshot(ctx context.Context, path string) (Backup, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Backup{}, err
	}
	if _, err := os.Stat(path); err == nil {
		return Backup{}, fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Backup{}, err
	}
	// VACUUM INTO takes no bound parameters; quote the path as a literal.
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(path, "'", "''")+"'"); err != nil {
		return Backup{}, fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return Backup{}, err
	}
	// Turso leaves an empty write-ahead log next to the snapshot; the
	// snapshot file is complete on its own.
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() == 0 {
		os.Remove(path + "-wal")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Backup{}, err
	}
	return Backup{Path: path, Size: info.Size(), Created: time.Now()}, nil
}

// record notes a backup for status reports. Failing to record does not undo
// a backup that was written, so it is only logged.
func (s *Store) record(ctx context.Context, b Backup, reason string) {
	err := s.q.RecordBackup(ctx, query.RecordBackupParams{Path: b.Path, Size: b.Size, Reason: reason, CreatedAt: b.Created.Unix()})
	if err != nil {
		s.log.Warn("backup written but not recorded", "path", b.Path, "err", err)
	}
}

// Status describes the database.
type Status struct {
	Path       string
	Size       int64 // database file plus write-ahead log
	Integrity  string
	Tokens     int64
	LastBackup *Backup
}

// Status reports the database's size, integrity and last backup.
func (s *Store) Status(ctx context.Context) (Status, error) {
	st := Status{Path: s.path}
	for _, p := range []string{s.path, s.path + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			st.Size += info.Size()
		}
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&st.Integrity); err != nil {
		return st, fmt.Errorf("integrity check: %w", err)
	}
	n, err := s.q.CountTokens(ctx)
	if err != nil {
		return st, err
	}
	st.Tokens = n
	last, err := s.q.LastBackup(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return st, err
	default:
		st.LastBackup = &Backup{Path: last.Path, Size: last.Size, Reason: last.Reason, Created: time.Unix(last.CreatedAt, 0)}
	}
	return st, nil
}
