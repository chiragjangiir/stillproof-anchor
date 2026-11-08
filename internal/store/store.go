// Package store keeps claims and batches in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// ErrNotFound means no claim or key has that id.
var ErrNotFound = errors.New("not found")

// Store is the service's state.
type Store struct{ db *sql.DB }

// Batch is one tree of claims and its anchoring state.
type Batch struct {
	ID       int64
	Root     []byte
	Size     int
	Tx       string // latest submitted transaction, empty until one is sent
	Account  string // the account that anchored it
	Anchored bool
	Ledger   uint32
	ClosedAt time.Time
	Proof    []byte // Externalize bundle for Tx (SPEC.md §8.2), nil until fetched
}

const schema = `
CREATE TABLE IF NOT EXISTS batches (
  id        INTEGER PRIMARY KEY,
  root      BLOB NOT NULL,
  size      INTEGER NOT NULL,
  tx        TEXT NOT NULL DEFAULT '',
  account   TEXT NOT NULL DEFAULT '',
  anchored  INTEGER NOT NULL DEFAULT 0,
  ledger    INTEGER NOT NULL DEFAULT 0,
  closed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS claims (
  seq      INTEGER PRIMARY KEY,
  id       TEXT NOT NULL UNIQUE,
  leaf     BLOB NOT NULL,
  signed   BLOB NOT NULL,
  batch    INTEGER REFERENCES batches(id),
  idx      INTEGER
);
CREATE INDEX IF NOT EXISTS claims_unbatched ON claims(seq) WHERE batch IS NULL;
CREATE INDEX IF NOT EXISTS claims_batch ON claims(batch, idx);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialising avoids SQLITE_BUSY.
	if err := createSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := migrateV1(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Add records a signed claim, indexed by its file hash when it parses.
// Adding the same claim again is a no-op.
func (s *Store) Add(ctx context.Context, id string, leaf, signed []byte) error {
	var sha sql.NullString
	if h, err := fileHash(signed); err == nil {
		sha = sql.NullString{String: h, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO claims (id, leaf, signed, sha256) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, leaf, signed, sha)
	return err
}

// Pending returns the open batch, if any: built but not yet anchored.
func (s *Store) Pending(ctx context.Context) (*Batch, error) {
	return s.batch(ctx, `SELECT id, root, size, tx, account, anchored, ledger, closed_at, proof FROM batches WHERE anchored = 0 ORDER BY id LIMIT 1`)
}

// Cut moves up to max unbatched claims, oldest first, into a new batch
// whose root is computed by root from their leaves. It returns nil when
// nothing is waiting.
func (s *Store) Cut(ctx context.Context, max int, root func(leaves [][]byte) []byte) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT seq, leaf FROM claims WHERE batch IS NULL ORDER BY seq LIMIT ?`, max)
	if err != nil {
		return nil, err
	}
	var seqs []int64
	var leaves [][]byte
	for rows.Next() {
		var seq int64
		var leaf []byte
		if err := rows.Scan(&seq, &leaf); err != nil {
			_ = rows.Close()
			return nil, err
		}
		seqs, leaves = append(seqs, seq), append(leaves, leaf)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, nil
	}
	b := &Batch{Root: root(leaves), Size: len(leaves)}
	res, err := tx.ExecContext(ctx, `INSERT INTO batches (root, size) VALUES (?, ?)`, b.Root, b.Size)
	if err != nil {
		return nil, err
	}
	if b.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	for i, seq := range seqs {
		if _, err := tx.ExecContext(ctx, `UPDATE claims SET batch = ?, idx = ? WHERE seq = ?`, b.ID, i, seq); err != nil {
			return nil, err
		}
	}
	return b, tx.Commit()
}

// Submitted records the transaction most recently sent for a batch.
func (s *Store) Submitted(ctx context.Context, batch int64, txHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE batches SET tx = ? WHERE id = ?`, txHash, batch)
	return err
}

// Anchored marks a batch anchored by txHash, from account, in ledger.
func (s *Store) Anchored(ctx context.Context, batch int64, txHash, account string, ledger uint32, closedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE batches SET tx = ?, account = ?, anchored = 1, ledger = ?, closed_at = ? WHERE id = ?`,
		txHash, account, ledger, closedAt.Unix(), batch)
	return err
}

// Lookup returns a claim's signed text, its batch (nil while unbatched)
// and its index in that batch.
func (s *Store) Lookup(ctx context.Context, id string) (signed []byte, b *Batch, index int, err error) {
	var batch sql.NullInt64
	var idx sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT signed, batch, idx FROM claims WHERE id = ?`, id).Scan(&signed, &batch, &idx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, ErrNotFound
	}
	if err != nil || !batch.Valid {
		return signed, nil, 0, err
	}
	b, err = s.batch(ctx, `SELECT id, root, size, tx, account, anchored, ledger, closed_at, proof FROM batches WHERE id = ?`, batch.Int64)
	return signed, b, int(idx.Int64), err
}

// Leaves returns a batch's leaves in tree order.
func (s *Store) Leaves(ctx context.Context, batch int64) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT leaf FROM claims WHERE batch = ? ORDER BY idx`, batch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out [][]byte
	for rows.Next() {
		var leaf []byte
		if err := rows.Scan(&leaf); err != nil {
			return nil, err
		}
		out = append(out, leaf)
	}
	return out, rows.Err()
}

func (s *Store) batch(ctx context.Context, q string, args ...any) (*Batch, error) {
	var b Batch
	var closed int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&b.ID, &b.Root, &b.Size, &b.Tx, &b.Account, &b.Anchored, &b.Ledger, &closed, &b.Proof)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if closed != 0 {
		b.ClosedAt = time.Unix(closed, 0).UTC()
	}
	return &b, nil
}
