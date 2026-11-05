package anchor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stillproof-labs/stillproof-anchor/internal/store"
)

// flaky drops the first `drop` transactions it is sent.
type flaky struct {
	drop, built int
	landed      map[string]bool
	roots       map[string][32]byte
}

func (f *flaky) Account() string { return "G" }
func (f *flaky) Build(_ context.Context, root [32]byte) (string, string, error) {
	f.built++
	h := fmt.Sprintf("tx%d", f.built)
	f.roots[h] = root
	return h, h, nil
}
func (f *flaky) Send(_ context.Context, env string) error {
	if f.drop > 0 {
		f.drop--
		return nil
	}
	f.landed[env] = true
	return nil
}
func (f *flaky) Status(_ context.Context, h string) (Status, error) {
	if f.landed[h] {
		return Status{State: "SUCCESS", Ledger: 9, ClosedAt: time.Unix(1, 0)}, nil
	}
	return Status{State: "NOT_FOUND"}, nil
}

func newBatcher(t *testing.T, c Chain) *Batcher {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Batcher{Store: st, Chain: c, MaxBatch: 10, Expiry: 20 * time.Millisecond, Poll: time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestADroppedTransactionIsRebuiltWithTheSameRoot(t *testing.T) {
	f := &flaky{drop: 2, landed: map[string]bool{}, roots: map[string][32]byte{}}
	b := newBatcher(t, f)
	ctx := context.Background()
	if err := b.Store.Add(ctx, "a", make([]byte, 32), []byte("a")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := b.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p, err := b.Store.Pending(ctx)
	if err != nil || p != nil {
		t.Fatalf("still pending after the third send: %+v %v", p, err)
	}
	if f.built != 3 || f.roots["tx1"] != f.roots["tx3"] {
		t.Fatalf("built %d transactions; roots differ: %v", f.built, f.roots["tx1"] != f.roots["tx3"])
	}
	_, batch, idx, err := b.Store.Lookup(ctx, "a")
	if err != nil || !batch.Anchored || batch.Tx != "tx3" || batch.Ledger != 9 || idx != 0 || batch.Account != "G" {
		t.Fatalf("%+v %v", batch, err)
	}
}

func TestARestartPicksUpTheOpenBatch(t *testing.T) {
	f := &flaky{landed: map[string]bool{}, roots: map[string][32]byte{}}
	b := newBatcher(t, f)
	ctx := context.Background()
	_ = b.Store.Add(ctx, "a", make([]byte, 32), []byte("a"))
	batch, err := b.Store.Cut(ctx, 10, root)
	if err != nil {
		t.Fatal(err)
	}
	// The previous process recorded tx0 and crashed; tx0 did land.
	f.landed["tx0"] = true
	_ = b.Store.Submitted(ctx, batch.ID, "tx0")
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.built != 0 {
		t.Fatalf("rebuilt %d transactions for a batch that had already landed", f.built)
	}
	if _, got, _, _ := b.Store.Lookup(ctx, "a"); !got.Anchored || got.Tx != "tx0" {
		t.Fatalf("%+v", got)
	}
}
