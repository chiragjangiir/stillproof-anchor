// Package api serves the anchoring service's HTTP API (SPEC.md §7, §8.4).
package api

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/stillproof-labs/stillproof-anchor/internal/store"
	"github.com/stillproof-labs/stillproof/attest"
	"github.com/stillproof-labs/stillproof/claim"
	"github.com/stillproof-labs/stillproof/merkle"
	"github.com/stillproof-labs/stillproof/receipt"
)

// Request body limits. A v0 signed claim is under 500 bytes; an attestation
// chain or object is a few kilobytes.
const (
	maxClaim = 2048
	maxKey   = 64 << 10
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Server handles requests.
type Server struct {
	Store   *store.Store
	Network string
	Log     *slog.Logger
	AppIDs  []string // accepted App Attest ids, "TEAMID.bundle"

	// Attest verifies a key's attestation and returns its level.
	// Nil means attest.Verify with AppIDs.
	Attest func(a attest.Attestation, key []byte) (level string, err error)
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/claims", s.submit)
	mux.HandleFunc("GET /v1/receipts/{id}", s.receipt)
	mux.HandleFunc("GET /v1/receipts", s.receipts)
	mux.HandleFunc("POST /v1/keys", s.registerKey)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return cors(mux)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxClaim))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "claim too large"})
		return
	}
	signed, err := claim.Parse(body)
	if err == nil {
		err = signed.Verify()
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	leaf := claim.LeafHash(body)
	id := hex.EncodeToString(leaf[:])
	if err := s.Store.Add(r.Context(), id, leaf[:], body); err != nil {
		s.Log.Error("store", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) receipt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be 64 lowercase hex characters"})
		return
	}
	rc, err := s.build(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown claim"})
	case err != nil:
		s.Log.Error("receipt", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
	case rc == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	default:
		writeJSON(w, http.StatusOK, rc)
	}
}

// receipts finds every receipt for a file by its SHA-256 (SPEC.md §8.4).
func (s *Server) receipts(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.URL.Query().Get("sha256"))
	if !idPattern.MatchString(sha) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sha256 must be 64 hex characters"})
		return
	}
	ids, err := s.Store.ByFile(r.Context(), sha)
	if err != nil {
		s.Log.Error("by file", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown file"})
		return
	}
	out := []*receipt.Receipt{}
	for _, id := range ids {
		rc, err := s.build(r.Context(), id)
		if err != nil {
			s.Log.Error("receipt", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
			return
		}
		if rc != nil {
			out = append(out, rc)
		}
	}
	if len(out) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": out})
}

// build returns the receipt for claim id, or nil while it is pending.
func (s *Server) build(ctx context.Context, id string) (*receipt.Receipt, error) {
	signed, batch, index, err := s.Store.Lookup(ctx, id)
	if err != nil || batch == nil || !batch.Anchored {
		return nil, err
	}
	leaves, err := s.Store.Leaves(ctx, batch.ID)
	if err != nil {
		return nil, err
	}
	hs := make([]merkle.Hash, len(leaves))
	for i, l := range leaves {
		copy(hs[i][:], l)
	}
	proof := []string{}
	for _, h := range merkle.Proof(hs, index) {
		proof = append(proof, hex.EncodeToString(h[:]))
	}
	rc := &receipt.Receipt{
		Claim: string(signed), Index: index, Size: batch.Size, Proof: proof, Root: hex.EncodeToString(batch.Root),
		Anchor: receipt.Anchor{Network: s.Network, Account: batch.Account, Tx: batch.Tx, Ledger: batch.Ledger, ClosedAt: batch.ClosedAt},
	}
	if rc.Attestation, err = s.attestation(ctx, signed); err != nil {
		return nil, err
	}
	if rc.Attestation != nil {
		rc.V = 1
	}
	return rc, nil
}

// attestation returns the registered attestation for a claim's key, if any.
func (s *Server) attestation(ctx context.Context, signed []byte) (*attest.Attestation, error) {
	c, err := claim.Parse(signed)
	if err != nil {
		return nil, err
	}
	key, err := c.Key.Bytes()
	if err != nil {
		return nil, err
	}
	raw, _, err := s.Store.Key(ctx, hex.EncodeToString(key))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a attest.Attestation
	return &a, json.Unmarshal(raw, &a)
}

// registerKey stores a key's hardware attestation (SPEC.md §8.1, §8.4).
func (s *Server) registerKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key         string              `json:"key"`
		Attestation *attest.Attestation `json:"attestation"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKey))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
		return
	}
	key, err := hex.DecodeString(req.Key)
	if err == nil && len(key) == 65 {
		_, err = ecdh.P256().NewPublicKey(key)
	}
	if err != nil || len(key) != 65 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key must be a 65-byte uncompressed P-256 public key in hex"})
		return
	}
	if req.Attestation == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attestation is required"})
		return
	}
	verify := s.Attest
	if verify == nil {
		verify = func(a attest.Attestation, key []byte) (string, error) {
			return attest.Verify(a, key, attest.Options{AppIDs: s.AppIDs})
		}
	}
	level, err := verify(*req.Attestation, key)
	if err == nil && (level == "" || level == attest.LevelSoftware) {
		err = fmt.Errorf("attestation gives level %q, not hardware", level)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	raw, err := json.Marshal(req.Attestation)
	if err == nil {
		err = s.Store.AddKey(r.Context(), hex.EncodeToString(key), raw, level, time.Now())
	}
	if err != nil {
		s.Log.Error("add key", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cors lets browser verifiers and apps on other origins call the API.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
