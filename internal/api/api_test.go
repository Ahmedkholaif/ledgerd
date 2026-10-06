package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ahmedkholaif/ledgerd/internal/api"
	"github.com/Ahmedkholaif/ledgerd/internal/ledger"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := ledger.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE entries, transfers, accounts`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(ledger.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, idemKey string, body any, out any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, &buf)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp
}

func TestTransferFlowOverHTTP(t *testing.T) {
	srv := newServer(t)
	var world, alice ledger.Account
	call(t, srv, "POST", "/v1/accounts", "", map[string]any{"name": "world", "currency": "USD", "allow_negative": true}, &world)
	call(t, srv, "POST", "/v1/accounts", "", map[string]any{"name": "alice", "currency": "USD"}, &alice)

	body := map[string]any{"description": "top-up", "postings": []map[string]any{
		{"account_id": world.ID, "amount": -1500}, {"account_id": alice.ID, "amount": 1500},
	}}
	if r := call(t, srv, "POST", "/v1/transfers", "topup-1", body, nil); r.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", r.StatusCode)
	}
	r := call(t, srv, "POST", "/v1/transfers", "topup-1", body, nil)
	if r.StatusCode != http.StatusOK || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: status %d, header %q", r.StatusCode, r.Header.Get("Idempotent-Replayed"))
	}

	var got ledger.Account
	call(t, srv, "GET", "/v1/accounts/"+alice.ID, "", nil, &got)
	if got.Balance != 1500 {
		t.Fatalf("balance %d, want 1500", got.Balance)
	}

	overdraw := map[string]any{"postings": []map[string]any{
		{"account_id": alice.ID, "amount": -5000}, {"account_id": world.ID, "amount": 5000},
	}}
	var errResp struct{ Error struct{ Code string } }
	if r := call(t, srv, "POST", "/v1/transfers", "overdraw", overdraw, &errResp); r.StatusCode != http.StatusConflict || errResp.Error.Code != "insufficient_funds" {
		t.Fatalf("overdraw: status %d code %q", r.StatusCode, errResp.Error.Code)
	}
	if r := call(t, srv, "POST", "/v1/transfers", "", body, nil); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing key: status %d", r.StatusCode)
	}

	var list struct{ Accounts []ledger.Account }
	call(t, srv, "GET", "/v1/accounts", "", nil, &list)
	if len(list.Accounts) != 2 || list.Accounts[0].Name != "world" {
		t.Fatalf("list accounts: %+v", list.Accounts)
	}

	var audit struct{ Consistent bool }
	call(t, srv, "GET", "/v1/audit", "", nil, &audit)
	if !audit.Consistent {
		t.Fatal("audit reports inconsistency")
	}
}
