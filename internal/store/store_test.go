package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stillproof-labs/stillproof/claim"
)

// v0Schema is the schema before receipt v1.
const v0Schema = `
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
CREATE TABLE claims (
  seq      INTEGER PRIMARY KEY,
  id       TEXT NOT NULL UNIQUE,
  leaf     BLOB NOT NULL,
  signed   BLOB NOT NULL,
  batch    INTEGER REFERENCES batches(id),
  idx      INTEGER
);
CREATE INDEX claims_unbatched ON claims(seq) WHERE batch IS NULL;
CREATE INDEX claims_batch ON claims(batch, idx);
`

func signed(t *testing.T, file string) (id string, leaf, text []byte, sha string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h := sha256.Sum256([]byte(file))
	s, err := claim.Sign(claim.Claim{SHA256: h, Type: "image/jpeg", Captured: 1, Key: &key.PublicKey}, key)
	if err != nil {
		t.Fatal(err)
	}
	l := claim.LeafHash(s.Bytes())
	return s.ID(), l[:], s.Bytes(), hex.EncodeToString(h[:])
}

func TestOpenUpgradesV0Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, v0Schema); err != nil {
		t.Fatal(err)
	}
	idA, leafA, textA, shaA := signed(t, "a")
	idB, leafB, textB, _ := signed(t, "b")
	for _, c := range []struct {
		id         string
		leaf, text []byte
	}{{idA, leafA, textA}, {idB, leafB, textB}, {"junk", make([]byte, 32), []byte("not a claim")}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO claims (id, leaf, signed) VALUES (?, ?, ?)`, c.id, c.leaf, c.text); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	for range 2 { // upgrading, then reopening an upgraded database
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := s.ByFile(ctx, shaA)
		if err != nil || len(ids) != 1 || ids[0] != idA {
			t.Fatalf("ByFile after upgrade: %v %v", ids, err)
		}
		if _, _, err := s.Key(ctx, "00"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("keys table: %v", err)
		}
		var plan string
		_ = s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM claims WHERE sha256 = ?`, shaA).Scan(new(int), new(int), new(int), &plan)
		if plan != "SEARCH claims USING INDEX claims_sha256 (sha256=?)" {
			t.Fatalf("lookup by file doesn't use the index: %q", plan)
		}
		_ = s.Close()
	}
}

func TestByFileAndKeys(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Two devices sign the same file: both claims, oldest first.
	id1, leaf1, text1, sha := signed(t, "same")
	id2, leaf2, text2, _ := signed(t, "same")
	for _, c := range [][3][]byte{{[]byte(id1), leaf1, text1}, {[]byte(id2), leaf2, text2}} {
		if err := s.Add(ctx, string(c[0]), c[1], c[2]); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.ByFile(ctx, sha)
	if err != nil || len(ids) != 2 || ids[0] != id1 || ids[1] != id2 {
		t.Fatalf("ByFile: %v %v", ids, err)
	}
	if ids, _ := s.ByFile(ctx, hex.EncodeToString(make([]byte, 32))); len(ids) != 0 {
		t.Fatalf("unknown file: %v", ids)
	}

	// The first attestation registered for a key stays.
	if err := s.AddKey(ctx, "k", []byte(`{"kind":"a"}`), "tee", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey(ctx, "k", []byte(`{"kind":"b"}`), "strongbox", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, level, err := s.Key(ctx, "k")
	if err != nil || string(a) != `{"kind":"a"}` || level != "tee" {
		t.Fatalf("Key: %s %s %v", a, level, err)
	}
}
