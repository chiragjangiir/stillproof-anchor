package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/stillproof-labs/stillproof/claim"
)

// Receipt v1 schema (SPEC.md §8.1, §8.4): registered keys, and each claim's
// file hash for lookup by file. Each statement stands alone so other
// migrations merge beside it.
const keysTable = `
CREATE TABLE IF NOT EXISTS keys (
  key         TEXT PRIMARY KEY,
  attestation TEXT NOT NULL,
  level       TEXT NOT NULL,
  created     INTEGER NOT NULL
)`

const sha256Index = `CREATE INDEX IF NOT EXISTS claims_sha256 ON claims(sha256)`

// migrateV1 adds the keys table and the claims.sha256 column, back-filling
// it from the signed claims already stored.
func migrateV1(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, keysTable); err != nil {
		return err
	}
	has, err := hasColumn(ctx, db, "claims", "sha256")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.ExecContext(ctx, `ALTER TABLE claims ADD COLUMN sha256 TEXT`); err != nil {
			return err
		}
	}
	if err := backfillSHA256(ctx, db); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, sha256Index)
	return err
}

func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func backfillSHA256(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT seq, signed FROM claims WHERE sha256 IS NULL`)
	if err != nil {
		return err
	}
	fill := map[int64]string{}
	for rows.Next() {
		var seq int64
		var signed []byte
		if err := rows.Scan(&seq, &signed); err != nil {
			_ = rows.Close()
			return err
		}
		if h, err := fileHash(signed); err == nil {
			fill[seq] = h
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for seq, h := range fill {
		if _, err := tx.ExecContext(ctx, `UPDATE claims SET sha256 = ? WHERE seq = ?`, h, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// fileHash is the hex SHA-256 of the file a signed claim is about.
func fileHash(signed []byte) (string, error) {
	s, err := claim.Parse(signed)
	if err != nil {
		return "", fmt.Errorf("store: %w", err)
	}
	return hex.EncodeToString(s.SHA256[:]), nil
}

// ByFile returns the ids of the claims about a file, oldest first.
func (s *Store) ByFile(ctx context.Context, sha256 string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM claims WHERE sha256 = ? ORDER BY seq`, sha256)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AddKey registers a key's attestation and level. A key keeps the first
// attestation registered for it; registering it again is a no-op.
func (s *Store) AddKey(ctx context.Context, key string, attestation []byte, level string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO keys (key, attestation, level, created) VALUES (?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`,
		key, string(attestation), level, now.Unix())
	return err
}

// Key returns a registered key's attestation JSON and level, or ErrNotFound.
func (s *Store) Key(ctx context.Context, key string) (attestation []byte, level string, err error) {
	var a string
	err = s.db.QueryRowContext(ctx, `SELECT attestation, level FROM keys WHERE key = ?`, key).Scan(&a, &level)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return []byte(a), level, err
}
