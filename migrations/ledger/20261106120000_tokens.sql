-- +goose Up

-- Task 3.4 (OQ-15): token purchase, lots and token holds. Additions to
-- data model §5.
--
-- A service wallet's holds reserve tokens, not money: held_tokens counts
-- them, and must never exceed the tokens left in the wallet's open lots.
-- held_minor stays for money holds (main and bonus wallets). Money in a
-- service wallet moves only with its lots, so balance_minor always
-- equals the value of the lots' remaining tokens.
ALTER TABLE ledger.balances
  ADD COLUMN held_tokens numeric(18,6) NOT NULL DEFAULT 0 CHECK (held_tokens >= 0);
GRANT UPDATE (held_tokens) ON ledger.balances TO ledger_service;

-- A token hold (tokens set) records how many tokens a capture used.
-- What it adds to held_tokens: tokens while active, captured_tokens once
-- captured until settled, nothing after.
ALTER TABLE ledger.holds
  ADD COLUMN captured_tokens numeric(18,6),
  ADD CONSTRAINT holds_tokens_positive CHECK (tokens > 0),
  ADD CONSTRAINT holds_captured_tokens_iff CHECK ((status = 'captured' AND tokens IS NOT NULL) = (captured_tokens IS NOT NULL)),
  ADD CONSTRAINT holds_captured_tokens_within CHECK (captured_tokens > 0 AND captured_tokens <= tokens);

ALTER TABLE ledger.token_lots
  ADD CONSTRAINT token_lots_bought_positive CHECK (tokens_bought > 0),
  ADD CONSTRAINT token_lots_remaining_within CHECK (tokens_remaining <= tokens_bought),
  ADD CONSTRAINT token_lots_price_positive CHECK (minor_per_token > 0);

-- The wallet guard (3.2) also refuses a service wallet holding more
-- tokens than its open lots have left. It now fires on token_lots too,
-- so a draw that leaves fewer tokens than are held fails at commit.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger.check_wallet_not_negative() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  b record;
  tokens_left numeric;
BEGIN
  SELECT bl.balance_minor, bl.held_minor, bl.held_tokens, a.type
    INTO b
    FROM ledger.balances bl JOIN ledger.accounts a ON a.id = bl.account_id
   WHERE bl.account_id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: balance of account % not visible to the wallet check', NEW.account_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF ledger.is_client_wallet(b.type)
     AND (b.balance_minor < 0 OR b.held_minor > b.balance_minor) THEN
    RAISE EXCEPTION 'ledger: wallet % would go below zero (balance %, held %)',
      NEW.account_id, b.balance_minor, b.held_minor
      USING ERRCODE = 'check_violation';
  END IF;
  IF b.held_tokens > 0 THEN
    SELECT coalesce(sum(tokens_remaining), 0) INTO tokens_left
      FROM ledger.token_lots WHERE account_id = NEW.account_id;
    IF b.held_tokens > tokens_left THEN
      RAISE EXCEPTION 'ledger: wallet % would hold % tokens with only % left',
        NEW.account_id, b.held_tokens, tokens_left
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER token_lots_wallet_not_negative
  AFTER INSERT OR UPDATE ON ledger.token_lots
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_wallet_not_negative();

-- +goose Down
DROP TRIGGER token_lots_wallet_not_negative ON ledger.token_lots;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger.check_wallet_not_negative() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  b record;
BEGIN
  SELECT bl.balance_minor, bl.held_minor, a.type
    INTO b
    FROM ledger.balances bl JOIN ledger.accounts a ON a.id = bl.account_id
   WHERE bl.account_id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: balance of account % not visible to the wallet check', NEW.account_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF ledger.is_client_wallet(b.type)
     AND (b.balance_minor < 0 OR b.held_minor > b.balance_minor) THEN
    RAISE EXCEPTION 'ledger: wallet % would go below zero (balance %, held %)',
      NEW.account_id, b.balance_minor, b.held_minor
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
ALTER TABLE ledger.token_lots
  DROP CONSTRAINT token_lots_price_positive,
  DROP CONSTRAINT token_lots_remaining_within,
  DROP CONSTRAINT token_lots_bought_positive;
ALTER TABLE ledger.holds
  DROP CONSTRAINT holds_captured_tokens_within,
  DROP CONSTRAINT holds_captured_tokens_iff,
  DROP CONSTRAINT holds_tokens_positive,
  DROP COLUMN captured_tokens;
REVOKE UPDATE (held_tokens) ON ledger.balances FROM ledger_service;
ALTER TABLE ledger.balances DROP COLUMN held_tokens;
