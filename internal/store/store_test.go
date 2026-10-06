package store

import (
	"context"
	"log/slog"
	"testing"
)

// TestOpenEmptyDatabase checks that migrations create a working schema and
// that reopening an up-to-date database neither migrates nor backs up.
func TestOpenEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	for range 2 {
		s, err := Open(ctx, dir, log)
		if err != nil {
			t.Fatal(err)
		}
		st, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Integrity != "ok" || st.LastBackup != nil {
			t.Errorf("status after open: integrity %q, last backup %+v", st.Integrity, st.LastBackup)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
