// Package api serves the anchoring service's HTTP API (SPEC.md §7).
package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/stillproof-labs/stillproof-anchor/internal/store"
	"github.com/stillproof-labs/stillproof/claim"
	"github.com/stillproof-labs/stillproof/merkle"
	"github.com/stillproof-labs/stillproof/receipt"
)

// maxClaim bounds a request body; a v0 signed claim is under 500 bytes.
const maxClaim = 2048

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Server handles requests.
type Server struct {
	Store   *store.Store
	Network string
	Log     *slog.Logger
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/claims", s.submit)
	mux.HandleFunc("GET /v1/receipts/{id}", s.receipt)
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
	signed, batch, index, err := s.Store.Lookup(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown claim"})
		return
	case err != nil:
		s.Log.Error("lookup", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
		return
	case batch == nil || !batch.Anchored:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	}
	leaves, err := s.Store.Leaves(r.Context(), batch.ID)
	if err != nil {
		s.Log.Error("leaves", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failed"})
		return
	}
	hs := make([]merkle.Hash, len(leaves))
	for i, l := range leaves {
		copy(hs[i][:], l)
	}
	proof := []string{}
	for _, h := range merkle.Proof(hs, index) {
		proof = append(proof, hex.EncodeToString(h[:]))
	}
	rc := receipt.Receipt{
		Claim: string(signed), Index: index, Size: batch.Size, Proof: proof, Root: hex.EncodeToString(batch.Root),
		Anchor: receipt.Anchor{Network: s.Network, Account: batch.Account, Tx: batch.Tx, Ledger: batch.Ledger, ClosedAt: batch.ClosedAt},
	}
	if batch.Proof != nil {
		rc.V, rc.Anchor.Proof = 1, batch.Proof // SPEC.md §8.2
	}
	writeJSON(w, http.StatusOK, rc)
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
