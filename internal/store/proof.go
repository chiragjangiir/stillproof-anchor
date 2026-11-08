package store

import (
	"context"
	"database/sql"
	"time"
)

// The validator proof column (SPEC.md §8.2): one nullable column on
// batches, added to databases created before it.
const proofColumn = `ALTER TABLE batches ADD COLUMN proof BLOB`

// createSchema creates the tables and adds the columns later versions need.
func createSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('batches') WHERE name = 'proof'`).Scan(&n)
	if err != nil || n > 0 {
		return err
	}
	_, err = db.ExecContext(ctx, proofColumn)
	return err
}

// Unproved returns up to max anchored batches without a proof, oldest first.
func (s *Store) Unproved(ctx context.Context, max int) ([]Batch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root, size, tx, account, ledger, closed_at FROM batches WHERE anchored = 1 AND proof IS NULL ORDER BY id LIMIT ?`, max)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Batch
	for rows.Next() {
		b := Batch{Anchored: true}
		var closed int64
		if err := rows.Scan(&b.ID, &b.Root, &b.Size, &b.Tx, &b.Account, &b.Ledger, &closed); err != nil {
			return nil, err
		}
		b.ClosedAt = time.Unix(closed, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// Proved stores an anchored batch's proof. It is a no-op for a batch that
// isn't anchored or already has one.
func (s *Store) Proved(ctx context.Context, batch int64, proof []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE batches SET proof = ? WHERE id = ? AND anchored = 1 AND proof IS NULL`, proof, batch)
	return err
}
