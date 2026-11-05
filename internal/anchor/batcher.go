// Package anchor cuts claims into batches and anchors each batch's root on
// Stellar (SPEC.md §4).
package anchor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/stillproof-labs/stillproof-anchor/internal/store"
	"github.com/stillproof-labs/stillproof/merkle"
)

// Status is a submitted transaction's outcome.
type Status struct {
	State    string // SUCCESS, FAILED or NOT_FOUND
	Ledger   uint32
	ClosedAt time.Time
}

// Chain builds, sends and tracks anchor transactions.
type Chain interface {
	// Build returns a signed anchor transaction for root and its hash.
	Build(ctx context.Context, root [32]byte) (envelope, hash string, err error)
	Send(ctx context.Context, envelope string) error
	Status(ctx context.Context, hash string) (Status, error)
	Account() string
}

// Batcher anchors one batch at a time.
type Batcher struct {
	Store    *store.Store
	Chain    Chain
	MaxBatch int           // claims per batch
	Expiry   time.Duration // a sent transaction not seen after this is presumed dead
	Poll     time.Duration // how often to check a sent transaction
	Log      *slog.Logger

	sentAt map[string]time.Time
}

// Run ticks every interval until ctx ends.
func (b *Batcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := b.Tick(ctx); err != nil && ctx.Err() == nil {
			b.Log.Warn("anchoring", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick finishes the open batch, or cuts a new one, and anchors it.
// It returns once the batch is anchored or its transaction is still in flight.
func (b *Batcher) Tick(ctx context.Context) error {
	batch, err := b.Store.Pending(ctx)
	if err != nil {
		return err
	}
	if batch == nil {
		if batch, err = b.Store.Cut(ctx, b.MaxBatch, root); err != nil || batch == nil {
			return err
		}
		b.Log.Info("batch", "id", batch.ID, "claims", batch.Size)
	}
	if batch.Tx != "" {
		switch st, err := b.settle(ctx, batch, batch.Tx); {
		case err != nil:
			return err
		case st != dead:
			return nil // anchored, or still in flight from an earlier tick
		}
	}
	var r [32]byte
	copy(r[:], batch.Root)
	env, hash, err := b.Chain.Build(ctx, r)
	if err != nil {
		return err
	}
	// Record before sending, so a crash in between can't lose track of it.
	if err := b.Store.Submitted(ctx, batch.ID, hash); err != nil {
		return err
	}
	if b.sentAt == nil {
		b.sentAt = map[string]time.Time{}
	}
	b.sentAt[hash] = time.Now()
	if err := b.Chain.Send(ctx, env); err != nil {
		return err
	}
	for {
		switch st, err := b.settle(ctx, batch, hash); {
		case err != nil:
			return err
		case st != waiting:
			return nil // anchored, or dead and rebuilt on the next tick
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.Poll):
		}
	}
}

type outcome int

const (
	anchored outcome = iota
	waiting          // sent, not yet seen, not yet expired
	dead             // failed, or never seen before expiry: send a new one
)

// settle checks hash against the network and records batch if it anchored.
func (b *Batcher) settle(ctx context.Context, batch *store.Batch, hash string) (outcome, error) {
	st, err := b.Chain.Status(ctx, hash)
	if err != nil {
		return waiting, err
	}
	switch st.State {
	case "SUCCESS":
		b.Log.Info("anchored", "batch", batch.ID, "tx", hash, "ledger", st.Ledger)
		delete(b.sentAt, hash)
		return anchored, b.Store.Anchored(ctx, batch.ID, hash, b.Chain.Account(), st.Ledger, st.ClosedAt)
	case "FAILED":
		delete(b.sentAt, hash)
		return dead, nil
	case "NOT_FOUND":
		if sent, ok := b.sentAt[hash]; ok && time.Since(sent) <= b.Expiry {
			return waiting, nil
		}
		delete(b.sentAt, hash)
		return dead, nil
	}
	return waiting, errors.New("unknown transaction state " + st.State)
}

func root(leaves [][]byte) []byte {
	hs := make([]merkle.Hash, len(leaves))
	for i, l := range leaves {
		copy(hs[i][:], l)
	}
	r := merkle.Root(hs)
	return r[:]
}
