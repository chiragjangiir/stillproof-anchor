package anchor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stillproof-labs/stillproof/receipt"
)

const passphrase = "Test SDF Network ; September 2015"

// fakeExnode answers like exnode serve: status (404 until the ledger is
// archived), then a bundle for whatever transaction it is asked about.
type fakeExnode struct {
	status  atomic.Int32 // 0 means 200
	claimed string       // if set, the bundle claims this transaction instead
	network string       // if set, the bundle's network
	noTxset bool
	format  string // if set, the bundle's format
	hits    atomic.Int32
	last    atomic.Value // *http.Request
}

func (f *fakeExnode) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.last.Store(r)
		if s := f.status.Load(); s != 0 {
			http.Error(w, `{"error":"checkpoint not published"}`, int(s))
			return
		}
		tx, network, txset, format := r.URL.Query().Get("tx"), passphrase, `"transactions":"T",`, receipt.BundleFormat
		if f.claimed != "" {
			tx = f.claimed
		}
		if f.network != "" {
			network = f.network
		}
		if f.noTxset {
			txset = ""
		}
		if f.format != "" {
			format = f.format
		}
		fmt.Fprintf(w, `{"format":%q,"network":%q,"ledger":"L","scp":"S","next_scp":"NS","next_transactions":"NT","results":"R",%s"claims":[{"kind":"transaction","tx_hash":%q}]}`,
			format, network, txset, tx)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func anchoredBatch(t *testing.T) *Batcher {
	t.Helper()
	b := newBatcher(t, &flaky{landed: map[string]bool{}, roots: map[string][32]byte{}})
	ctx := context.Background()
	if err := b.Store.Add(ctx, "a", make([]byte, 32), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, batch, _, _ := b.Store.Lookup(ctx, "a"); batch == nil || !batch.Anchored {
		t.Fatal("not anchored")
	}
	return b
}

func prover(b *Batcher, url string) *Prover {
	return &Prover{Store: b.Store, Exnode: url, Network: "testnet", Passphrase: passphrase, Log: b.Log}
}

func proofOf(t *testing.T, b *Batcher) []byte {
	t.Helper()
	_, batch, _, err := b.Store.Lookup(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	return batch.Proof
}

func TestProverRetriesUntilTheLedgerIsArchived(t *testing.T) {
	b := anchoredBatch(t)
	ctx := context.Background()
	ex := &fakeExnode{}
	p := prover(b, ex.serve(t)+"/")

	for _, s := range []int32{http.StatusNotFound, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadGateway} {
		ex.status.Store(s)
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if proofOf(t, b) != nil {
			t.Fatalf("HTTP %d: proof stored", s)
		}
	}
	ex.status.Store(0)
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	proof := proofOf(t, b)
	if !strings.Contains(string(proof), `"tx_hash":"tx1"`) {
		t.Fatalf("proof %s", proof)
	}
	r := ex.last.Load().(*http.Request)
	if r.URL.Path != "/v1/ledgers/9/bundle" || r.URL.Query().Get("tx") != "tx1" || r.URL.Query().Get("txset") != "true" {
		t.Fatalf("asked for %s", r.URL)
	}
	if left, _ := b.Store.Unproved(ctx, 10); len(left) != 0 {
		t.Fatalf("still unproved: %v", left)
	}
	n := ex.hits.Load()
	if err := p.Tick(ctx); err != nil || ex.hits.Load() != n {
		t.Fatalf("asked again for a proved batch: %v", err)
	}
}

func TestProverStopsAtTheFirstUnarchivedLedger(t *testing.T) {
	b := anchoredBatch(t)
	ctx := context.Background()
	if err := b.Store.Add(ctx, "b", make([]byte, 32), []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := b.Store.Unproved(ctx, 10); len(u) != 2 {
		t.Fatalf("want 2 anchored batches, have %d", len(u))
	}
	ex := &fakeExnode{}
	ex.status.Store(http.StatusNotFound)
	if err := prover(b, ex.serve(t)).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if ex.hits.Load() != 1 {
		t.Fatalf("%d requests, want 1", ex.hits.Load())
	}
}

func TestProverRejectsWrongBundles(t *testing.T) {
	for name, ex := range map[string]*fakeExnode{
		"another transaction": {claimed: "tx2"},
		"another network":     {network: "Public Global Stellar Network ; September 2015"},
		"no transaction set":  {noTxset: true},
		"v1":                  {format: "externalize/bundle/v1"},
	} {
		b := anchoredBatch(t)
		if err := prover(b, ex.serve(t)).Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if p := proofOf(t, b); p != nil {
			t.Errorf("%s: stored %s", name, p)
		}
	}
}

type fakeCheck struct {
	tx  *receipt.Tx
	err error
	got receipt.Anchor
}

func (f *fakeCheck) Check(_ context.Context, a receipt.Anchor) (*receipt.Tx, error) {
	f.got = a
	return f.tx, f.err
}

func TestProverChecksTheBundleWhenItCan(t *testing.T) {
	ctx := context.Background()
	good := func(b *Batcher) *receipt.Tx {
		_, batch, _, _ := b.Store.Lookup(ctx, "a")
		var root [32]byte
		copy(root[:], batch.Root)
		return &receipt.Tx{Successful: true, Source: batch.Account, MemoHash: &root, Ledger: batch.Ledger, ClosedAt: batch.ClosedAt}
	}
	cases := map[string]struct {
		edit   func(tx *receipt.Tx) (*receipt.Tx, error)
		stored bool
	}{
		"proven":       {func(tx *receipt.Tx) (*receipt.Tx, error) { return tx, nil }, true},
		"rejected":     {func(*receipt.Tx) (*receipt.Tx, error) { return nil, errors.New("rejected") }, false},
		"unchecked":    {func(*receipt.Tx) (*receipt.Tx, error) { return nil, fmt.Errorf("%w: no CLI", receipt.ErrUnchecked) }, false},
		"other memo":   {func(tx *receipt.Tx) (*receipt.Tx, error) { tx.MemoHash = &[32]byte{1}; return tx, nil }, false},
		"failed":       {func(tx *receipt.Tx) (*receipt.Tx, error) { tx.Successful = false; return tx, nil }, false},
		"other ledger": {func(tx *receipt.Tx) (*receipt.Tx, error) { tx.Ledger++; return tx, nil }, false},
	}
	for name, c := range cases {
		b := anchoredBatch(t)
		chk := &fakeCheck{}
		chk.tx, chk.err = c.edit(good(b))
		p := prover(b, (&fakeExnode{}).serve(t))
		p.Check = chk
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := proofOf(t, b) != nil; got != c.stored {
			t.Errorf("%s: stored %v, want %v", name, got, c.stored)
		}
		if chk.got.Tx != "tx1" || chk.got.Network != "testnet" || chk.got.Account != "G" || len(chk.got.Proof) == 0 {
			t.Errorf("%s: checked %+v", name, chk.got)
		}
	}
}
