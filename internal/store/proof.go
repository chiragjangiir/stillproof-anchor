package store

import (
	"context"
	"database/sql"
	"time"
)

// The validator proof column (SPEC.md §8.2): one nullable column on
// batches, added to databases created before it.
const proofColumn = `ALTER TABLE batches ADD COLUMN proof BLOB`

// dropOldProofs clears proofs in any format but externalize/bundle/v2, so
// they are fetched again: v1 bundles don't certify the whole ledger header.
const dropOldProofs = `UPDATE batches SET proof = NULL WHERE proof IS NOT NULL AND
  (CASE WHEN json_valid(CAST(proof AS TEXT)) THEN json_extract(CAST(proof AS TEXT), '$.format') END) IS NOT 'externalize/bundle/v2'`

// createSchema creates the tables, adds the columns later versions need and
// drops proofs in older formats.
func createSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('batches') WHERE name = 'proof'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx, proofColumn); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, dropOldProofs)
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
