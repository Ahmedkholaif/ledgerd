// Package api exposes the ledger over a small JSON HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Ahmedkholaif/ledgerd/internal/ledger"
)

type Server struct {
	ledger *ledger.Ledger
	log    *slog.Logger
}

func New(l *ledger.Ledger, log *slog.Logger) *Server { return &Server{ledger: l, log: log} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/accounts", s.createAccount)
	mux.HandleFunc("GET /v1/accounts", s.listAccounts)
	mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", s.listEntries)
	mux.HandleFunc("POST /v1/transfers", s.createTransfer)
	mux.HandleFunc("GET /v1/transfers/{id}", s.getTransfer)
	mux.HandleFunc("GET /v1/audit", s.audit)
	return s.logRequests(mux)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in ledger.NewAccount
	if !decode(w, r, &in) {
		return
	}
	a, err := s.ledger.CreateAccount(r.Context(), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	accounts, err := s.ledger.ListAccounts(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.ledger.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.ledger.ListEntries(r.Context(), r.PathValue("id"), after, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := map[string]any{"entries": entries, "next_after": nil}
	if len(entries) > 0 {
		resp["next_after"] = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req ledger.TransferRequest
	if !decode(w, r, &req) {
		return
	}
	req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	t, replayed, err := s.ledger.Transfer(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, t)
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := s.ledger.GetTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	m, err := s.ledger.Audit(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"consistent": len(m) == 0, "mismatches": m})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, ledger.ErrInvalid):
		status, code = http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, ledger.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, ledger.ErrInsufficientFunds):
		status, code = http.StatusConflict, "insufficient_funds"
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, context.Canceled):
		status, code = 499, "canceled"
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
		msg = "internal error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "bad_json", "message": err.Error()}})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.InfoContext(r.Context(), "http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}
