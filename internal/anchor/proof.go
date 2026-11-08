package anchor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stillproof-labs/stillproof/receipt"

	"github.com/stillproof-labs/stillproof-anchor/internal/store"
)

// maxBundle bounds a bundle download; a bundle with a full testnet
// transaction set is usually well under 1 MB.
const maxBundle = 16 << 20

// Prover fetches each anchored batch's Externalize bundle, with its
// transaction set, from an exnode (SPEC.md §8.2). A bundle exists only once
// the anchor's ledger is in a history archive, so it retries until then.
//
// It asks for GET /v1/ledgers/{ledger}/bundle?tx=…&txset=true: exnode's
// /v1/transactions/{hash}/bundle has no txset option, and needs an RPC
// that still has the transaction.
type Prover struct {
	Store      *store.Store
	Exnode     string // base URL, e.g. http://127.0.0.1:8081
	Network    string // "testnet" or "mainnet", as in receipts
	Passphrase string // the network's passphrase, which the bundle must name

	// Check, if set, must accept a bundle before it is stored, so a bad one
	// never reaches receipts. An error wrapping receipt.ErrUnchecked
	// leaves the batch for the next try.
	Check receipt.ProofChecker

	HTTP *http.Client // nil means a client with a 2 min timeout
	Log  *slog.Logger
}

// errNotYet means the exnode can't build the bundle yet.
var errNotYet = errors.New("bundle not available yet")

// Run tries every interval until ctx ends.
func (p *Prover) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := p.Tick(ctx); err != nil && ctx.Err() == nil {
			p.Log.Warn("proving", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick fetches proofs for anchored batches that lack one, oldest first. It
// stops at the first batch whose bundle isn't available yet: later ledgers
// aren't archived either. A batch that fails otherwise is logged and
// retried on the next tick.
func (p *Prover) Tick(ctx context.Context) error {
	batches, err := p.Store.Unproved(ctx, 50)
	if err != nil {
		return err
	}
	for _, b := range batches {
		proof, err := p.prove(ctx, b)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errNotYet):
			p.Log.Debug("proof not available yet", "batch", b.ID, "ledger", b.Ledger, "err", err)
			return nil
		case err != nil:
			p.Log.Warn("proof", "batch", b.ID, "ledger", b.Ledger, "err", err)
			continue
		}
		if err := p.Store.Proved(ctx, b.ID, proof); err != nil {
			return err
		}
		p.Log.Info("proved", "batch", b.ID, "tx", b.Tx, "ledger", b.Ledger)
	}
	return nil
}

// prove fetches and checks the bundle for b.
func (p *Prover) prove(ctx context.Context, b store.Batch) ([]byte, error) {
	body, err := p.fetch(ctx, b)
	if err != nil {
		return nil, err
	}
	// The exnode is untrusted and verifiers check the bundle; this keeps
	// a wrong answer out of receipts.
	var head struct {
		Format       string `json:"format"`
		Network      string `json:"network"`
		Transactions string `json:"transactions"`
		Claims       []struct {
			Kind   string `json:"kind"`
			TxHash string `json:"tx_hash"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("exnode: ledger %d: %w", b.Ledger, err)
	}
	if head.Format != "externalize/bundle/v1" || head.Network != p.Passphrase || head.Transactions == "" ||
		len(head.Claims) != 1 || head.Claims[0].Kind != "transaction" || head.Claims[0].TxHash != b.Tx {
		return nil, fmt.Errorf("exnode: ledger %d: not a bundle for transaction %s with its transaction set", b.Ledger, b.Tx)
	}
	if p.Check == nil {
		return body, nil
	}
	a := receipt.Anchor{Network: p.Network, Account: b.Account, Tx: b.Tx, Ledger: b.Ledger, ClosedAt: b.ClosedAt, Proof: body}
	tx, err := p.Check.Check(ctx, a)
	switch {
	case errors.Is(err, receipt.ErrUnchecked):
		return nil, fmt.Errorf("%w: %w", errNotYet, err)
	case err != nil:
		return nil, fmt.Errorf("exnode: ledger %d: %w", b.Ledger, err)
	case !tx.Successful || tx.Source != b.Account || tx.MemoHash == nil || hex.EncodeToString(tx.MemoHash[:]) != hex.EncodeToString(b.Root) ||
		tx.Ledger != b.Ledger || !tx.ClosedAt.Equal(b.ClosedAt):
		return nil, fmt.Errorf("exnode: ledger %d: the bundle doesn't prove batch %d's anchor", b.Ledger, b.ID)
	}
	return body, nil
}

// fetch returns the exnode's bundle for b, or an error wrapping errNotYet
// if the ledger isn't archived yet or the exnode asks to come back later.
func (p *Prover) fetch(ctx context.Context, b store.Batch) ([]byte, error) {
	u := strings.TrimRight(p.Exnode, "/") + "/v1/ledgers/" + strconv.FormatUint(uint64(b.Ledger), 10) +
		"/bundle?" + url.Values{"tx": {b.Tx}, "txset": {"true"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: exnode: ledger %d: HTTP %d", errNotYet, b.Ledger, resp.StatusCode)
	default:
		return nil, fmt.Errorf("exnode: ledger %d: HTTP %d", b.Ledger, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBundle {
		return nil, fmt.Errorf("exnode: ledger %d: bundle over %d bytes", b.Ledger, maxBundle)
	}
	return body, nil
}
