// Package ledger implements a double-entry ledger on PostgreSQL.
//
// Money moves only through transfers. A transfer is a set of postings that
// must net to zero per currency, is applied atomically, and is idempotent on a
// client-supplied key. Concurrency safety comes from locking the touched
// account rows in a fixed order (no deadlocks), and the core invariants are
// enforced twice: here in Go for clear errors, and in the schema (CHECK
// constraints plus a deferred constraint trigger) so that no code path,
// including a manual SQL session, can corrupt the books.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxPostings bounds the size of a single transfer.
const MaxPostings = 64

var (
	ErrNotFound            = errors.New("not found")
	ErrInvalid             = errors.New("invalid request")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var (
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

type Account struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Currency      string    `json:"currency"`
	AllowNegative bool      `json:"allow_negative"`
	Balance       int64     `json:"balance"`
	CreatedAt     time.Time `json:"created_at"`
}

type NewAccount struct {
	Name          string `json:"name"`
	Currency      string `json:"currency"`
	AllowNegative bool   `json:"allow_negative"`
}

// Posting moves Amount minor units into (positive) or out of (negative) an account.
type Posting struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
}

type TransferRequest struct {
	IdempotencyKey string    `json:"-"`
	Description    string    `json:"description"`
	Postings       []Posting `json:"postings"`
}

type Entry struct {
	ID           int64     `json:"id"`
	TransferID   string    `json:"transfer_id"`
	AccountID    string    `json:"account_id"`
	Amount       int64     `json:"amount"`
	Currency     string    `json:"currency"`
	BalanceAfter int64     `json:"balance_after"`
	CreatedAt    time.Time `json:"created_at"`
}

type Transfer struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Description    string    `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
	Entries        []Entry   `json:"entries"`
}

type Ledger struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Ledger { return &Ledger{pool: pool} }

func (l *Ledger) CreateAccount(ctx context.Context, in NewAccount) (Account, error) {
	if in.Name == "" || len(in.Name) > 200 {
		return Account{}, invalid("name is required and must be at most 200 characters")
	}
	if !currencyRe.MatchString(in.Currency) {
		return Account{}, invalid("currency must be a 3-letter ISO 4217 code such as USD")
	}
	var a Account
	err := l.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency, allow_negative) VALUES ($1, $2, $3)
		RETURNING id::text, name, currency, allow_negative, balance, created_at`,
		in.Name, in.Currency, in.AllowNegative,
	).Scan(&a.ID, &a.Name, &a.Currency, &a.AllowNegative, &a.Balance, &a.CreatedAt)
	return a, err
}

func (l *Ledger) GetAccount(ctx context.Context, id string) (Account, error) {
	if !uuidRe.MatchString(id) {
		return Account{}, ErrNotFound
	}
	var a Account
	err := l.pool.QueryRow(ctx, `
		SELECT id::text, name, currency, allow_negative, balance, created_at
		FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.Currency, &a.AllowNegative, &a.Balance, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// ListEntries returns an account's journal in order, using keyset pagination:
// pass the last entry ID you saw as after.
func (l *Ledger) ListEntries(ctx context.Context, accountID string, after int64, limit int) ([]Entry, error) {
	if _, err := l.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := l.pool.Query(ctx, `
		SELECT id, transfer_id::text, account_id::text, amount, currency, balance_after, created_at
		FROM entries WHERE account_id = $1 AND id > $2 ORDER BY id LIMIT $3`,
		accountID, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEntry)
}

func (l *Ledger) GetTransfer(ctx context.Context, id string) (Transfer, error) {
	if !uuidRe.MatchString(id) {
		return Transfer{}, ErrNotFound
	}
	return getTransfer(ctx, l.pool, `id = $1`, id)
}

// Transfer applies a balanced set of postings atomically.
//
// Retrying with the same idempotency key and the same request returns the
// original transfer with replayed=true and moves no money. Reusing a key for a
// different request fails with ErrIdempotencyConflict. A rejected transfer
// (e.g. insufficient funds) does not consume its key.
func (l *Ledger) Transfer(ctx context.Context, req TransferRequest) (t Transfer, replayed bool, err error) {
	if err := validate(req); err != nil {
		return Transfer{}, false, err
	}
	hash := requestHash(req)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Transfer{}, false, err
	}
	defer tx.Rollback(ctx)

	// Claim the idempotency key. If another transaction holds the same key,
	// this blocks on the unique index until that one commits or rolls back.
	var transferID string
	err = tx.QueryRow(ctx, `
		INSERT INTO transfers (idempotency_key, request_hash, description) VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text`,
		req.IdempotencyKey, hash[:], req.Description,
	).Scan(&transferID)
	if errors.Is(err, pgx.ErrNoRows) {
		return l.replay(ctx, req.IdempotencyKey, hash[:])
	}
	if err != nil {
		return Transfer{}, false, err
	}

	// Lock every touched account in ascending ID order. Any two transfers
	// therefore acquire overlapping locks in the same order and cannot deadlock.
	ids := make([]string, len(req.Postings))
	for i, p := range req.Postings {
		ids[i] = p.AccountID
	}
	sort.Strings(ids)

	type acct struct {
		currency      string
		allowNegative bool
		balance       int64
	}
	accounts := make(map[string]*acct, len(ids))
	rows, err := tx.Query(ctx, `
		SELECT id::text, currency, allow_negative, balance FROM accounts
		WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return Transfer{}, false, err
	}
	for rows.Next() {
		var id string
		a := &acct{}
		if err := rows.Scan(&id, &a.currency, &a.allowNegative, &a.balance); err != nil {
			rows.Close()
			return Transfer{}, false, err
		}
		accounts[id] = a
	}
	if err := rows.Err(); err != nil {
		return Transfer{}, false, err
	}
	for _, id := range ids {
		if accounts[id] == nil {
			return Transfer{}, false, fmt.Errorf("%w: account %s", ErrNotFound, id)
		}
	}

	net := map[string]int64{}
	for _, p := range req.Postings {
		net[accounts[p.AccountID].currency] += p.Amount
	}
	for cur, sum := range net {
		if sum != 0 {
			return Transfer{}, false, invalid("postings must net to zero per currency; %s nets to %d", cur, sum)
		}
	}

	batch := &pgx.Batch{}
	for _, p := range req.Postings {
		a := accounts[p.AccountID]
		next := a.balance + p.Amount
		if (p.Amount > 0 && next < a.balance) || (p.Amount < 0 && next > a.balance) {
			return Transfer{}, false, invalid("balance overflow on account %s", p.AccountID)
		}
		if next < 0 && !a.allowNegative {
			return Transfer{}, false, fmt.Errorf("%w: account %s has %d, needs %d", ErrInsufficientFunds, p.AccountID, a.balance, -p.Amount)
		}
		a.balance = next
		batch.Queue(`UPDATE accounts SET balance = $2 WHERE id = $1`, p.AccountID, next)
		batch.Queue(`INSERT INTO entries (transfer_id, account_id, amount, currency, balance_after)
			VALUES ($1, $2, $3, $4, $5)`, transferID, p.AccountID, p.Amount, a.currency, next)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Transfer{}, false, err
	}

	t, err = getTransfer(ctx, tx, `id = $1`, transferID)
	if err != nil {
		return Transfer{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, false, err
	}
	return t, false, nil
}

func (l *Ledger) replay(ctx context.Context, key string, hash []byte) (Transfer, bool, error) {
	var stored []byte
	if err := l.pool.QueryRow(ctx, `SELECT request_hash FROM transfers WHERE idempotency_key = $1`, key).Scan(&stored); err != nil {
		return Transfer{}, false, err
	}
	if string(stored) != string(hash) {
		return Transfer{}, false, ErrIdempotencyConflict
	}
	t, err := getTransfer(ctx, l.pool, `idempotency_key = $1`, key)
	return t, true, err
}

// Mismatch describes a broken invariant found by Audit.
type Mismatch struct {
	Kind     string `json:"kind"` // "account" or "currency"
	ID       string `json:"id"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
}

// Audit recomputes every balance from the journal and checks that each
// currency nets to zero across the whole ledger. An empty result means the
// books are consistent.
func (l *Ledger) Audit(ctx context.Context) ([]Mismatch, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT 'account', a.id::text, COALESCE(sum(e.amount), 0)::bigint, a.balance
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		GROUP BY a.id
		HAVING a.balance <> COALESCE(sum(e.amount), 0)
		UNION ALL
		SELECT 'currency', currency, 0, sum(balance)::bigint
		FROM accounts GROUP BY currency HAVING sum(balance) <> 0`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Mismatch, error) {
		var m Mismatch
		err := r.Scan(&m.Kind, &m.ID, &m.Expected, &m.Actual)
		return m, err
	})
}

func validate(req TransferRequest) error {
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 255 {
		return invalid("an idempotency key of 1-255 characters is required")
	}
	if len(req.Description) > 1000 {
		return invalid("description must be at most 1000 characters")
	}
	if len(req.Postings) < 2 || len(req.Postings) > MaxPostings {
		return invalid("a transfer needs between 2 and %d postings", MaxPostings)
	}
	seen := map[string]bool{}
	for _, p := range req.Postings {
		if !uuidRe.MatchString(p.AccountID) {
			return invalid("account_id %q is not a UUID", p.AccountID)
		}
		if seen[p.AccountID] {
			return invalid("account %s appears more than once", p.AccountID)
		}
		seen[p.AccountID] = true
		if p.Amount == 0 {
			return invalid("posting amounts must be non-zero")
		}
	}
	return nil
}

// requestHash fingerprints a transfer independently of posting order so that
// an equivalent retry is recognised as the same request.
func requestHash(req TransferRequest) [32]byte {
	ps := append([]Posting(nil), req.Postings...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].AccountID < ps[j].AccountID })
	b, _ := json.Marshal(struct {
		D string
		P []Posting
	}{req.Description, ps})
	return sha256.Sum256(b)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getTransfer(ctx context.Context, q querier, where string, arg any) (Transfer, error) {
	var t Transfer
	err := q.QueryRow(ctx, `SELECT id::text, idempotency_key, description, created_at FROM transfers WHERE `+where, arg).
		Scan(&t.ID, &t.IdempotencyKey, &t.Description, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ErrNotFound
	}
	if err != nil {
		return Transfer{}, err
	}
	rows, err := q.Query(ctx, `
		SELECT id, transfer_id::text, account_id::text, amount, currency, balance_after, created_at
		FROM entries WHERE transfer_id = $1 ORDER BY id`, t.ID)
	if err != nil {
		return Transfer{}, err
	}
	t.Entries, err = pgx.CollectRows(rows, scanEntry)
	return t, err
}

func scanEntry(r pgx.CollectableRow) (Entry, error) {
	var e Entry
	err := r.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.Amount, &e.Currency, &e.BalanceAfter, &e.CreatedAt)
	return e, err
}
