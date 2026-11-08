package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// beforeProofs is the schema before the proof column.
const beforeProofs = `
CREATE TABLE batches (
  id        INTEGER PRIMARY KEY,
  root      BLOB NOT NULL,
  size      INTEGER NOT NULL,
  tx        TEXT NOT NULL DEFAULT '',
  account   TEXT NOT NULL DEFAULT '',
  anchored  INTEGER NOT NULL DEFAULT 0,
  ledger    INTEGER NOT NULL DEFAULT 0,
  closed_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO batches (root, size) VALUES (x'00', 1);
`

func TestOpenAddsTheProofColumnToAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(beforeProofs); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	ctx := context.Background()
	for range 2 { // opening again is a no-op
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if u, err := s.Unproved(ctx, 10); err != nil || len(u) != 0 {
			t.Fatalf("unanchored batch listed: %+v %v", u, err)
		}
		_ = s.Close()
	}
}

func TestProofLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.Add(ctx, "a", make([]byte, 32), []byte("a")); err != nil {
		t.Fatal(err)
	}
	b, err := s.Cut(ctx, 10, func([][]byte) []byte { return make([]byte, 32) })
	if err != nil || b == nil {
		t.Fatalf("cut: %+v %v", b, err)
	}
	if err := s.Proved(ctx, b.ID, []byte("early")); err != nil {
		t.Fatal(err)
	}
	if _, got, _, _ := s.Lookup(ctx, "a"); got.Proof != nil {
		t.Fatalf("proof stored before anchoring: %s", got.Proof)
	}
	if err := s.Anchored(ctx, b.ID, "tx", "G", 7, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if u, err := s.Unproved(ctx, 10); err != nil || len(u) != 1 || u[0].ID != b.ID || u[0].Tx != "tx" || u[0].Account != "G" ||
		u[0].Ledger != 7 || !u[0].ClosedAt.Equal(time.Unix(1, 0)) || len(u[0].Root) != 32 {
		t.Fatalf("unproved: %+v %v", u, err)
	}
	if err := s.Proved(ctx, b.ID, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := s.Proved(ctx, b.ID, []byte("again")); err != nil {
		t.Fatal(err)
	}
	if _, got, _, _ := s.Lookup(ctx, "a"); string(got.Proof) != "{}" {
		t.Fatalf("proof %q", got.Proof)
	}
	if u, _ := s.Unproved(ctx, 10); len(u) != 0 {
		t.Fatalf("still unproved: %+v", u)
	}
}
