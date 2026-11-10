package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stillproof-labs/stillproof-anchor/internal/anchor"
	"github.com/stillproof-labs/stillproof-anchor/internal/store"
	"github.com/stillproof-labs/stillproof/claim"
	"github.com/stillproof-labs/stillproof/receipt"
)

const account = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"

// chain is an in-memory Stellar: every transaction lands in the next ledger.
type chain struct {
	ledger uint32
	txs    map[string]*receipt.Tx
	built  map[string][32]byte
}

func (c *chain) Account() string { return account }

func (c *chain) Build(_ context.Context, root [32]byte) (string, string, error) {
	c.ledger++
	hash := fmt.Sprintf("%064x", c.ledger)
	c.built[hash] = root
	return hash, hash, nil
}

func (c *chain) Send(_ context.Context, env string) error {
	root := c.built[env]
	c.txs[env] = &receipt.Tx{Successful: true, Source: account, MemoHash: &root, Ledger: c.ledger, ClosedAt: time.Unix(int64(1_800_000_000+c.ledger), 0).UTC()}
	return nil
}

func (c *chain) Status(_ context.Context, hash string) (anchor.Status, error) {
	tx, ok := c.txs[hash]
	if !ok {
		return anchor.Status{State: "NOT_FOUND"}, nil
	}
	return anchor.Status{State: "SUCCESS", Ledger: tx.Ledger, ClosedAt: tx.ClosedAt}, nil
}

func (c *chain) Transaction(_ context.Context, _, hash string) (*receipt.Tx, error) {
	if tx, ok := c.txs[hash]; ok {
		return tx, nil
	}
	return nil, receipt.ErrNotFound
}

func setup(t *testing.T) (*httptest.Server, *anchor.Batcher, *chain) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &chain{txs: map[string]*receipt.Tx{}, built: map[string][32]byte{}}
	b := &anchor.Batcher{Store: st, Chain: c, MaxBatch: 1000, Expiry: time.Minute, Poll: time.Millisecond, Log: log}
	srv := httptest.NewServer((&Server{Store: st, Network: "testnet", Log: log}).Handler())
	t.Cleanup(srv.Close)
	return srv, b, c
}

func signedClaim(t *testing.T, key *ecdsa.PrivateKey, file []byte) []byte {
	t.Helper()
	s, err := claim.Sign(claim.Claim{SHA256: sha256.Sum256(file), Type: "image/jpeg", Captured: time.Now().UnixMilli(), Key: &key.PublicKey}, key)
	if err != nil {
		t.Fatal(err)
	}
	return s.Bytes()
}

func post(t *testing.T, srv *httptest.Server, body []byte) (int, map[string]string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/claims", "text/plain", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, srv *httptest.Server, id string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/receipts/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestClaimsBecomeReceiptsThatVerify(t *testing.T) {
	srv, b, c := setup(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	files := make([][]byte, 5)
	ids := make([]string, len(files))
	for i := range files {
		files[i] = []byte(fmt.Sprintf("photo %d", i))
		code, out := post(t, srv, signedClaim(t, key, files[i]))
		if code != http.StatusAccepted || len(out["id"]) != 64 {
			t.Fatalf("submit %d: %d %v", i, code, out)
		}
		ids[i] = out["id"]
	}
	if code, _ := get(t, srv, ids[0]); code != http.StatusAccepted {
		t.Fatalf("before anchoring: %d, want 202", code)
	}
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		code, body := get(t, srv, id)
		if code != http.StatusOK {
			t.Fatalf("receipt %d: %d %s", i, code, body)
		}
		var r receipt.Receipt
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		if v := receipt.Verify(context.Background(), files[i], r, c); !v.OK() {
			t.Fatalf("receipt %d: %s: %s", i, v.Failed, v.Reason)
		}
		if v := receipt.Verify(context.Background(), files[(i+1)%len(files)], r, c); v.Failed != receipt.StepFile {
			t.Fatalf("receipt %d accepted another file", i)
		}
	}
	if len(c.txs) != 1 {
		t.Fatalf("%d anchor transactions for one batch", len(c.txs))
	}

	// A later claim goes in a new batch; an empty tick sends nothing.
	_, out := post(t, srv, signedClaim(t, key, []byte("later")))
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, srv, out["id"]); code != http.StatusOK || len(c.txs) != 2 {
		t.Fatalf("second batch: %d, %d transactions", code, len(c.txs))
	}
}

func TestSubmitRejectsBadClaimsAndIsIdempotent(t *testing.T) {
	srv, _, _ := setup(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := signedClaim(t, key, []byte("x"))
	_, a := post(t, srv, good)
	_, b := post(t, srv, good)
	if a["id"] == "" || a["id"] != b["id"] {
		t.Fatalf("resubmission gave %v then %v", a, b)
	}
	bad := bytes.Replace(good, []byte("image/jpeg"), []byte("image/png"), 1)
	if code, _ := post(t, srv, bad); code != http.StatusBadRequest {
		t.Fatalf("tampered claim: %d", code)
	}
	if code, _ := post(t, srv, bytes.Repeat([]byte("a"), 4096)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized claim: %d", code)
	}
	if code, _ := get(t, srv, "nothex"); code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := get(t, srv, hex.EncodeToString(make([]byte, 32))); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
}

func TestAProvedBatchGivesV1ReceiptsWithTheProof(t *testing.T) {
	srv, b, c := setup(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	file := []byte("photo")
	_, out := post(t, srv, signedClaim(t, key, file))
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r receipt.Receipt
	_, body := get(t, srv, out["id"])
	if err := json.Unmarshal(body, &r); err != nil || r.V != 0 || r.Anchor.Proof != nil {
		t.Fatalf("before the proof: %s %v", body, err)
	}
	proof := `{"format":"externalize/bundle/v2"}`
	if err := b.Store.Proved(context.Background(), 1, []byte(proof)); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, srv, out["id"])
	r = receipt.Receipt{}
	if err := json.Unmarshal(body, &r); err != nil || r.V != 1 || string(r.Anchor.Proof) != proof {
		t.Fatalf("after the proof: %s %v", body, err)
	}
	if v := receipt.Verify(context.Background(), file, r, c); !v.OK() {
		t.Fatalf("%s: %s", v.Failed, v.Reason)
	}
}

type errReader struct{ err error }

func (e errReader) Read(p []byte) (n int, err error) {
	return 0, e.err
}

func TestSubmitRejectsBodyReadErrorWithBadRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{Store: st, Network: "testnet", Log: log}

	req := httptest.NewRequest(http.MethodPost, "/v1/claims", errReader{err: errors.New("connection reset")})
	rec := httptest.NewRecorder()
	s.submit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for non-size read error, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["error"] != "body: connection reset" {
		t.Fatalf("expected error message 'body: connection reset', got: %s", rec.Body.String())
	}
}

