package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ahmedkholaif/ledgerd/internal/ledger"
)

// newLedger connects to TEST_DATABASE_URL, migrates and empties the schema.
func newLedger(t *testing.T) (*ledger.Ledger, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
	return ledger.New(pool), pool
}

func mustAccount(t *testing.T, l *ledger.Ledger, name, cur string, allowNeg bool) ledger.Account {
	t.Helper()
	a, err := l.CreateAccount(context.Background(), ledger.NewAccount{Name: name, Currency: cur, AllowNegative: allowNeg})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func move(l *ledger.Ledger, key, from, to string, amount int64) (ledger.Transfer, bool, error) {
	return l.Transfer(context.Background(), ledger.TransferRequest{
		IdempotencyKey: key,
		Postings:       []ledger.Posting{{AccountID: from, Amount: -amount}, {AccountID: to, Amount: amount}},
	})
}

func balance(t *testing.T, l *ledger.Ledger, id string) int64 {
	t.Helper()
	a, err := l.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func assertConsistent(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	m, err := l.Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("audit found mismatches: %+v", m)
	}
}

func TestTransferMovesMoney(t *testing.T) {
	l, _ := newLedger(t)
	world := mustAccount(t, l, "world", "USD", true)
	alice := mustAccount(t, l, "alice", "USD", false)
	bob := mustAccount(t, l, "bob", "USD", false)

	if _, _, err := move(l, "fund", world.ID, alice.ID, 10_000); err != nil {
		t.Fatal(err)
	}
	tr, replayed, err := move(l, "pay-1", alice.ID, bob.ID, 2_500)
	if err != nil || replayed {
		t.Fatalf("err=%v replayed=%v", err, replayed)
	}
	if len(tr.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(tr.Entries))
	}
	if got := balance(t, l, alice.ID); got != 7_500 {
		t.Fatalf("alice = %d, want 7500", got)
	}
	if got := balance(t, l, bob.ID); got != 2_500 {
		t.Fatalf("bob = %d, want 2500", got)
	}
	entries, err := l.ListEntries(context.Background(), alice.ID, 0, 10)
	if err != nil || len(entries) != 2 || entries[1].BalanceAfter != 7_500 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	assertConsistent(t, l)
}

func TestInsufficientFundsMovesNothing(t *testing.T) {
	l, _ := newLedger(t)
	alice := mustAccount(t, l, "alice", "EUR", false)
	bob := mustAccount(t, l, "bob", "EUR", false)

	_, _, err := move(l, "k", alice.ID, bob.ID, 1)
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if balance(t, l, alice.ID) != 0 || balance(t, l, bob.ID) != 0 {
		t.Fatal("balances changed after rejected transfer")
	}
	// A rejected transfer does not burn its key.
	world := mustAccount(t, l, "world", "EUR", true)
	move(l, "fund", world.ID, alice.ID, 5)
	if _, _, err := move(l, "k", alice.ID, bob.ID, 1); err != nil {
		t.Fatalf("retry after funding: %v", err)
	}
	assertConsistent(t, l)
}

func TestRejectsUnbalancedAndMixedCurrency(t *testing.T) {
	l, _ := newLedger(t)
	usd := mustAccount(t, l, "usd", "USD", true)
	eur := mustAccount(t, l, "eur", "EUR", true)
	usd2 := mustAccount(t, l, "usd2", "USD", true)

	cases := map[string][]ledger.Posting{
		"unbalanced":     {{AccountID: usd.ID, Amount: -10}, {AccountID: usd2.ID, Amount: 9}},
		"cross-currency": {{AccountID: usd.ID, Amount: -10}, {AccountID: eur.ID, Amount: 10}},
		"single posting": {{AccountID: usd.ID, Amount: 0}},
		"duplicate":      {{AccountID: usd.ID, Amount: -1}, {AccountID: usd.ID, Amount: 1}},
	}
	for name, ps := range cases {
		_, _, err := l.Transfer(context.Background(), ledger.TransferRequest{IdempotencyKey: name, Postings: ps})
		if !errors.Is(err, ledger.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// FX is expressed as one balanced transfer through per-currency clearing accounts.
	fxUSD := mustAccount(t, l, "fx-usd", "USD", true)
	fxEUR := mustAccount(t, l, "fx-eur", "EUR", true)
	_, _, err := l.Transfer(context.Background(), ledger.TransferRequest{IdempotencyKey: "fx", Postings: []ledger.Posting{
		{AccountID: usd.ID, Amount: -110}, {AccountID: fxUSD.ID, Amount: 110},
		{AccountID: fxEUR.ID, Amount: -100}, {AccountID: eur.ID, Amount: 100},
	}})
	if err != nil {
		t.Fatalf("fx transfer: %v", err)
	}
	assertConsistent(t, l)
}

func TestIdempotency(t *testing.T) {
	l, _ := newLedger(t)
	world := mustAccount(t, l, "world", "USD", true)
	alice := mustAccount(t, l, "alice", "USD", false)

	first, _, err := move(l, "same-key", world.ID, alice.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := move(l, "same-key", world.ID, alice.ID, 100)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay: err=%v replayed=%v id=%s want %s", err, replayed, again.ID, first.ID)
	}
	if got := balance(t, l, alice.ID); got != 100 {
		t.Fatalf("money moved twice: balance %d", got)
	}
	if _, _, err := move(l, "same-key", world.ID, alice.ID, 999); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
}

// Many clients racing with the same key must produce exactly one transfer.
func TestConcurrentSameIdempotencyKey(t *testing.T) {
	l, _ := newLedger(t)
	world := mustAccount(t, l, "world", "USD", true)
	alice := mustAccount(t, l, "alice", "USD", false)

	const n = 24
	ids := make([]string, n)
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, replayed, err := move(l, "race", world.ID, alice.ID, 50)
			if err != nil {
				t.Error(err)
				return
			}
			if !replayed {
				created.Add(1)
			}
			ids[i] = tr.ID
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("%d goroutines created a transfer, want exactly 1", created.Load())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("goroutines saw different transfers: %s vs %s", id, ids[0])
		}
	}
	if got := balance(t, l, alice.ID); got != 50 {
		t.Fatalf("balance %d, want 50", got)
	}
}

// Hammer a small set of accounts with random transfers from many goroutines.
// Afterwards no balance may be negative, nothing may deadlock, and the books
// must reconcile exactly.
func TestConcurrentTransfersPreserveInvariants(t *testing.T) {
	l, _ := newLedger(t)
	world := mustAccount(t, l, "world", "USD", true)
	const accounts, workers, perWorker = 8, 32, 40
	ids := make([]string, accounts)
	for i := range ids {
		ids[i] = mustAccount(t, l, fmt.Sprintf("acct-%d", i), "USD", false).ID
		if _, _, err := move(l, "fund-"+ids[i], world.ID, ids[i], 1_000); err != nil {
			t.Fatal(err)
		}
	}

	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for i := range perWorker {
				from, to := r.IntN(accounts), r.IntN(accounts-1)
				if to >= from {
					to++
				}
				_, _, err := move(l, fmt.Sprintf("w%d-%d", w, i), ids[from], ids[to], int64(1+r.IntN(400)))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ledger.ErrInsufficientFunds):
					rejected.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("%d transfers applied, %d rejected for insufficient funds", ok.Load(), rejected.Load())

	var total int64
	for _, id := range ids {
		b := balance(t, l, id)
		if b < 0 {
			t.Fatalf("account %s went negative: %d", id, b)
		}
		total += b
	}
	if total != accounts*1_000 {
		t.Fatalf("money was created or destroyed: total %d, want %d", total, accounts*1_000)
	}
	assertConsistent(t, l)
}

// The database rejects corruption even when the Go code is bypassed.
func TestSchemaEnforcesInvariants(t *testing.T) {
	l, pool := newLedger(t)
	ctx := context.Background()
	a := mustAccount(t, l, "a", "USD", true)
	b := mustAccount(t, l, "b", "USD", true)
	if _, _, err := move(l, "k", a.ID, b.ID, 10); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE entries SET amount = amount * 2`); err == nil {
		t.Error("entries were updated; want append-only error")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transfers`); err == nil {
		t.Error("transfers were deleted; want append-only error")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	tx.QueryRow(ctx, `INSERT INTO transfers (idempotency_key, request_hash) VALUES ('raw', '\x00') RETURNING id::text`).Scan(&id)
	tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, amount, currency, balance_after) VALUES ($1, $2, 500, 'USD', 0)`, id, a.ID)
	if err := tx.Commit(ctx); err == nil {
		t.Error("unbalanced transfer committed; want deferred constraint violation")
	}

	c := mustAccount(t, l, "c", "USD", false)
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = -1 WHERE id = $1`, c.ID); err == nil {
		t.Error("non-negative account went negative via raw SQL")
	}
	assertConsistent(t, l)
}
