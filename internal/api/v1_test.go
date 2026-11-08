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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stillproof-labs/stillproof-anchor/internal/anchor"
	"github.com/stillproof-labs/stillproof-anchor/internal/store"
	"github.com/stillproof-labs/stillproof/attest"
	"github.com/stillproof-labs/stillproof/receipt"
)

// fakeAttest accepts an attestation whose key_id is the hex key.
func fakeAttest(a attest.Attestation, key []byte) (string, error) {
	if a.Kind != attest.KindAppleAppAttest || a.KeyID != hex.EncodeToString(key) {
		return "", errors.Join(attest.ErrAttestation, errors.New("key mismatch"))
	}
	return attest.LevelSecureEnclave, nil
}

func setupV1(t *testing.T, verify func(attest.Attestation, []byte) (string, error)) (*httptest.Server, *anchor.Batcher, *chain) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &chain{txs: map[string]*receipt.Tx{}, built: map[string][32]byte{}}
	b := &anchor.Batcher{Store: st, Chain: c, MaxBatch: 1000, Expiry: time.Minute, Poll: time.Millisecond, Log: log}
	srv := httptest.NewServer((&Server{Store: st, Network: "testnet", Log: log, Attest: verify}).Handler())
	t.Cleanup(srv.Close)
	return srv, b, c
}

func pubHex(key *ecdsa.PrivateKey) string {
	b, _ := key.PublicKey.Bytes()
	return hex.EncodeToString(b)
}

func postKey(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/keys", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func lookup(t *testing.T, srv *httptest.Server, sha string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/v1/receipts?sha256=" + sha)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func TestRegisterKey(t *testing.T) {
	srv, _, _ := setupV1(t, fakeAttest)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k := pubHex(key)
	good := `{"key":"` + k + `","attestation":{"kind":"apple-app-attest","key_id":"` + k + `","object":"AA=="}}`
	for i := range 2 { // idempotent
		if code, body := postKey(t, srv, good); code != http.StatusNoContent {
			t.Fatalf("register %d: %d %s", i, code, body)
		}
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	notOnCurve := "04" + strings.Repeat("11", 64)
	for _, tc := range []struct{ name, body, reason string }{
		{"attestation for another key", `{"key":"` + pubHex(other) + `","attestation":{"kind":"apple-app-attest","key_id":"` + k + `"}}`, "key mismatch"},
		{"no attestation", `{"key":"` + k + `"}`, "attestation is required"},
		{"short key", `{"key":"04ab","attestation":{"kind":"android-key"}}`, "65-byte"},
		{"key not on the curve", `{"key":"` + notOnCurve + `","attestation":{"kind":"android-key"}}`, "65-byte"},
		{"not hex", `{"key":"zz","attestation":{"kind":"android-key"}}`, "65-byte"},
		{"unknown field", `{"key":"` + k + `","attestation":{"kind":"apple-app-attest","key_id":"` + k + `","extra":1}}`, "unknown field"},
		{"not JSON", `nope`, "body"},
	} {
		code, body := postKey(t, srv, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.reason) {
			t.Errorf("%s: %d %s, want 400 with %q", tc.name, code, body, tc.reason)
		}
	}
	if code, _ := postKey(t, srv, `{"key":"`+strings.Repeat("a", 70<<10)+`"}`); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", code)
	}
}

func TestRegisterKeyRejectsSoftwareLevel(t *testing.T) {
	srv, _, _ := setupV1(t, func(attest.Attestation, []byte) (string, error) { return attest.LevelSoftware, nil })
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if code, _ := postKey(t, srv, `{"key":"`+pubHex(key)+`","attestation":{"kind":"android-key"}}`); code != http.StatusBadRequest {
		t.Fatalf("software level accepted: %d", code)
	}
}

func TestRegisterKeyUsesAttestVerifyByDefault(t *testing.T) {
	srv, _, _ := setupV1(t, nil)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// An empty chain never verifies, whatever attest.Verify's state.
	code, body := postKey(t, srv, `{"key":"`+pubHex(key)+`","attestation":{"kind":"android-key","chain":[]}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "attestation") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestReceiptsCarryTheKeysAttestation(t *testing.T) {
	srv, b, c := setupV1(t, fakeAttest)
	hw, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sw, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k := pubHex(hw)
	if code, body := postKey(t, srv, `{"key":"`+k+`","attestation":{"kind":"apple-app-attest","key_id":"`+k+`","object":"AA=="}}`); code != http.StatusNoContent {
		t.Fatalf("register: %d %s", code, body)
	}
	_, hwOut := post(t, srv, signedClaim(t, hw, []byte("hw")))
	_, swOut := post(t, srv, signedClaim(t, sw, []byte("sw")))
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, file string
		v        int
		attested bool
	}{{hwOut["id"], "hw", 1, true}, {swOut["id"], "sw", 0, false}} {
		code, body := get(t, srv, tc.id)
		var r receipt.Receipt
		if err := json.Unmarshal(body, &r); code != http.StatusOK || err != nil {
			t.Fatalf("%s: %d %s", tc.file, code, body)
		}
		if r.V != tc.v || (r.Attestation != nil) != tc.attested {
			t.Fatalf("%s: v %d, attestation %v", tc.file, r.V, r.Attestation)
		}
		if tc.attested && (r.Attestation.Kind != attest.KindAppleAppAttest || r.Attestation.KeyID != k || r.Attestation.Object != "AA==") {
			t.Fatalf("attestation changed: %+v", r.Attestation)
		}
		if !tc.attested && bytes.Contains(body, []byte("attestation")) {
			t.Fatalf("v0 receipt has an attestation field: %s", body)
		}
		// The fake attestation may fail the signature step once the core
		// checks attestations; every other step must pass.
		if v := receipt.Verify(context.Background(), []byte(tc.file), r, c); !v.OK() && (!tc.attested || v.Failed != receipt.StepSignature) {
			t.Fatalf("%s: %s: %s", tc.file, v.Failed, v.Reason)
		}
	}
}

func TestReceiptsBySHA256(t *testing.T) {
	srv, b, c := setupV1(t, fakeAttest)
	file := []byte("photo")
	sum := sha256.Sum256(file)
	sha := hex.EncodeToString(sum[:])

	if code, _, _ := lookup(t, srv, sha); code != http.StatusNotFound {
		t.Fatalf("unknown file: %d", code)
	}
	for _, bad := range []string{"", "xyz", sha[:62], sha + "00"} {
		if code, _, _ := lookup(t, srv, bad); code != http.StatusBadRequest {
			t.Fatalf("sha256=%q: %d", bad, code)
		}
	}

	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, first := post(t, srv, signedClaim(t, k1, file))
	if code, _, body := lookup(t, srv, sha); code != http.StatusAccepted || !strings.Contains(string(body), "pending") {
		t.Fatalf("all pending: %d %s", code, body)
	}
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, second := post(t, srv, signedClaim(t, k2, file)) // pending: left out
	post(t, srv, signedClaim(t, k1, []byte("another photo")))

	code, h, body := lookup(t, srv, strings.ToUpper(sha))
	if code != http.StatusOK || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("lookup: %d %q %s", code, h.Get("Access-Control-Allow-Origin"), body)
	}
	var out struct{ Receipts []receipt.Receipt }
	if err := json.Unmarshal(body, &out); err != nil || len(out.Receipts) != 1 {
		t.Fatalf("one anchored, one pending: %s", body)
	}
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, body = lookup(t, srv, sha)
	out.Receipts = nil
	if err := json.Unmarshal(body, &out); err != nil || len(out.Receipts) != 2 {
		t.Fatalf("both anchored: %s", body)
	}
	for i, id := range []string{first["id"], second["id"]} { // oldest first
		r := out.Receipts[i]
		if got := hex.EncodeToString(sha256Leaf(r.Claim)); got != id {
			t.Fatalf("receipt %d is claim %s, want %s", i, got, id)
		}
		if v := receipt.Verify(context.Background(), file, r, c); !v.OK() {
			t.Fatalf("receipt %d: %s: %s", i, v.Failed, v.Reason)
		}
	}
}

func TestLookupPreflight(t *testing.T) {
	srv, _, _ := setupV1(t, fakeAttest)
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/receipts?sha256=00", nil)
	req.Header.Set("Origin", "https://verify.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "GET") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

func sha256Leaf(signed string) []byte {
	h := sha256.Sum256(append([]byte{0}, signed...))
	return h[:]
}
