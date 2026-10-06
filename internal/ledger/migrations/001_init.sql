-- Accounts hold a cached balance in minor units (e.g. cents). The cache is
-- updated in the same transaction as the entries that change it, and the
-- audit query in ledger.Audit proves the two never drift apart.
CREATE TABLE accounts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text        NOT NULL,
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    allow_negative boolean     NOT NULL DEFAULT false,
    balance        bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT balance_non_negative CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE transfers (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text        NOT NULL UNIQUE,
    request_hash    bytea       NOT NULL,
    description     text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE entries (
    id            bigserial   PRIMARY KEY,
    transfer_id   uuid        NOT NULL REFERENCES transfers (id),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
    amount        bigint      NOT NULL CHECK (amount <> 0),
    currency      char(3)     NOT NULL,
    balance_after bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, account_id)
);

CREATE INDEX entries_account_id_id ON entries (account_id, id);

-- The journal is append-only: corrections are new transfers, never edits.
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER entries_append_only BEFORE UPDATE OR DELETE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER transfers_append_only BEFORE UPDATE OR DELETE ON transfers
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- Double-entry invariant enforced by the database itself, not just the app:
-- at commit time every transfer must net to zero in each currency.
CREATE FUNCTION ledger_check_transfer_balanced() RETURNS trigger AS $$
DECLARE
    bad_currency char(3);
BEGIN
    SELECT currency INTO bad_currency
    FROM entries
    WHERE transfer_id = NEW.transfer_id
    GROUP BY currency
    HAVING sum(amount) <> 0
    LIMIT 1;

    IF bad_currency IS NOT NULL THEN
        RAISE EXCEPTION 'transfer % does not balance in %', NEW.transfer_id, bad_currency
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER entries_transfer_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_transfer_balanced();
