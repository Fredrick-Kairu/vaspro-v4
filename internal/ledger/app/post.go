package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// transactionsKey is the unique key a concurrent duplicate posting
// loses on (data model §5's UNIQUE NULLS NOT DISTINCT).
const transactionsKey = "transactions_tenant_id_idempotency_key_key"

// Post posts one balanced transaction (OQ-14): replayed when the key
// was used for the same posting, refused when for a different one; the
// accounts it names are opened on first use; it may not spend held
// money or touch a frozen or closed account.
func (s *Service) Post(ctx context.Context, req PostRequest) (PostResult, error) {
	if err := validatePostRequest(req); err != nil {
		return PostResult{}, err
	}
	var res PostResult
	run := func() error {
		return s.inOwnerTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.postInTx(ctx, tx, req)
			res = r
			return err
		})
	}
	err := s.withSystemAccounts(ctx, keysOf(req.Lines), run)
	if uniqueViolationOn(err, transactionsKey) {
		// A concurrent request with the same key committed first; this
		// run now answers as its replay.
		err = run()
	}
	return res, s.guardError(err)
}

func validatePostRequest(req PostRequest) error {
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 255 {
		return fmt.Errorf("%w: an idempotency key of 1 to 255 characters is required", ErrValidation)
	}
	if req.Description == "" {
		return fmt.Errorf("%w: a description is required", ErrValidation)
	}
	if (req.ReferenceType == "") != (req.ReferenceID == nil) {
		return fmt.Errorf("%w: a reference needs both its type and its ID", ErrValidation)
	}
	err := domain.ValidatePosting(domain.Posting{Type: req.Type, System: req.TenantID == nil, Internal: req.internal, Lines: req.Lines})
	if errors.Is(err, domain.ErrInvalidPosting) {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return err
}

// postInTx posts req in the caller's transaction, or answers it as a
// replay. CaptureHold reuses it for a capture's posting.
func (s *Service) postInTx(ctx context.Context, tx pgx.Tx, req PostRequest) (PostResult, error) {
	// Post validates before its transaction; the ledger's own postings
	// (purchase, settlement) come straight here, so every posting is
	// checked once more, the database's guards behind it.
	if err := validatePostRequest(req); err != nil {
		return PostResult{}, err
	}
	q := store.New(tx)

	existing, err := transactionByKey(ctx, q, req.TenantID, req.IdempotencyKey)
	if err == nil {
		return replayPosting(ctx, q, existing, req)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PostResult{}, fmt.Errorf("app: looking up the idempotency key: %w", err)
	}

	accts, err := resolveAccounts(ctx, q, req.TenantID, keysOf(req.Lines))
	if err != nil {
		return PostResult{}, err
	}
	var clientIDs []uuid.UUID
	for _, a := range accts {
		if a.Status != "open" {
			return PostResult{}, fmt.Errorf("%w: %s account %s is %s", ErrAccountNotOpen, a.Type, a.ID, a.Status)
		}
		if a.TenantID.Valid {
			clientIDs = append(clientIDs, a.ID)
		}
	}

	deltas, err := domain.NetDeltas(req.Lines)
	if err != nil {
		return PostResult{}, err
	}
	locked, err := lockBalances(ctx, q, clientIDs)
	if err != nil {
		return PostResult{}, err
	}
	for key, delta := range deltas {
		row, isClient := locked[accts[key].ID]
		if !isClient || !domain.IsClientWallet(key.Type) || delta >= 0 {
			continue
		}
		if available := row.BalanceMinor - row.HeldMinor; available+delta < 0 {
			return PostResult{}, fmt.Errorf("%w: %s holds %d available, the posting needs %d",
				ErrInsufficientBalance, key.Type, available, -delta)
		}
	}

	txnID, err := ids.New()
	if err != nil {
		return PostResult{}, err
	}
	if _, err := q.CreateTransaction(ctx, store.CreateTransactionParams{
		ID: txnID, TenantID: pgUUID(req.TenantID), Type: store.LedgerTxnType(req.Type),
		IdempotencyKey: req.IdempotencyKey, ReferenceType: text(req.ReferenceType),
		ReferenceID: pgUUID(req.ReferenceID), ReversesID: pgUUID(req.reverses),
		Description: req.Description, CreatedBy: pgUUID(req.CreatedBy),
	}); err != nil {
		return PostResult{}, fmt.Errorf("app: writing the transaction: %w", err)
	}

	// Lines go in account_id order, so the balance trigger locks rows in
	// the same order as lockBalances did (ADR 0010).
	posted := make([]PostedLine, len(req.Lines))
	for i, l := range req.Lines {
		posted[i] = PostedLine{AccountID: accts[l.Account].ID, Line: l}
	}
	slices.SortStableFunc(posted, func(a, b PostedLine) int { return compareUUID(a.AccountID, b.AccountID) })
	for _, p := range posted {
		params := store.CreateEntryParams{
			TransactionID: txnID, AccountID: p.AccountID, Direction: string(rune(p.Line.Direction)),
			AmountMinor: p.Line.AmountMinor, Currency: p.Line.Account.Currency,
		}
		if p.Line.Tokens != "" {
			params.Tokens = toNumeric(decimal.RequireFromString(p.Line.Tokens)) // validated by ValidatePosting
		}
		params.TaxCode = text(p.Line.TaxCode)
		if _, err := q.CreateEntry(ctx, params); err != nil {
			return PostResult{}, fmt.Errorf("app: writing a line: %w", err)
		}
	}

	if err := s.writeWalletCredited(ctx, tx, req, txnID, accts, deltas); err != nil {
		return PostResult{}, err
	}
	return PostResult{TransactionID: txnID, Lines: posted}, nil
}

// replayPosting answers a repeated key: the same type and the same
// lines (in any order) are a replay; anything else is a conflict.
func replayPosting(ctx context.Context, q *store.Queries, existing store.LedgerTransaction, req PostRequest) (PostResult, error) {
	rows, err := q.ListPostedLines(ctx, existing.ID)
	if err != nil {
		return PostResult{}, fmt.Errorf("app: reading the posted lines: %w", err)
	}
	posted := make([]PostedLine, len(rows))
	stored := map[domain.Line]int{}
	for i, r := range rows {
		l := domain.Line{
			Account:     keyOf(r.Type, r.ServiceCode, r.Rail, r.Currency),
			Direction:   domain.Side(r.Direction[0]),
			AmountMinor: r.AmountMinor,
		}
		if r.Tokens.Valid {
			l.Tokens = toDecimal(r.Tokens).String()
		}
		l.TaxCode = r.TaxCode.String
		posted[i] = PostedLine{AccountID: r.AccountID, Line: l}
		stored[l]++
	}
	for _, l := range req.Lines {
		if l.Tokens != "" {
			l.Tokens = decimal.RequireFromString(l.Tokens).String() // the stored lines' canonical form
		}
		stored[l]--
	}
	same := domain.TxnType(existing.Type) == req.Type && len(rows) == len(req.Lines)
	for _, n := range stored {
		same = same && n == 0
	}
	if !same {
		return PostResult{}, fmt.Errorf("%w: idempotency key %q was used for a different posting", ErrIdempotencyConflict, req.IdempotencyKey)
	}
	return PostResult{TransactionID: existing.ID, Lines: posted, Replayed: true}, nil
}

func transactionByKey(ctx context.Context, q *store.Queries, tenant *uuid.UUID, key string) (store.LedgerTransaction, error) {
	if tenant == nil {
		return q.GetSystemTransactionByKey(ctx, key)
	}
	return q.GetClientTransactionByKey(ctx, store.GetClientTransactionByKeyParams{TenantID: pgUUID(tenant), IdempotencyKey: key})
}

// writeWalletCredited writes wallet.credited for each main wallet a
// top-up or refund raised (data model §5's Events).
func (s *Service) writeWalletCredited(ctx context.Context, tx pgx.Tx, req PostRequest, txnID uuid.UUID,
	accts map[domain.AccountKey]store.LedgerAccount, deltas map[domain.AccountKey]int64) error {
	if req.TenantID == nil || (req.Type != domain.TxnTopup && req.Type != domain.TxnRefund) {
		return nil
	}
	for key, delta := range deltas {
		if key.Type != domain.MainWallet || delta <= 0 {
			continue
		}
		acct := accts[key].ID
		data := map[string]any{
			"tenant_id":        *req.TenantID,
			"account_id":       acct,
			"transaction_id":   txnID,
			"transaction_type": req.Type,
			"currency":         key.Currency,
			"amount_minor":     delta,
		}
		if req.ReferenceID != nil {
			data["reference_type"] = req.ReferenceType
			data["reference_id"] = *req.ReferenceID
		}
		eventID, err := ids.New()
		if err != nil {
			return err
		}
		env, err := platformevents.New(eventID, "wallet.credited", 1, *req.TenantID, s.clk.Now(), "", data)
		if err != nil {
			return fmt.Errorf("app: building wallet.credited: %w", err)
		}
		if err := outbox.Write(ctx, tx, "wallet", acct, env); err != nil {
			return err
		}
	}
	return nil
}

func keysOf(lines []domain.Line) []domain.AccountKey {
	keys := make([]domain.AccountKey, 0, len(lines))
	for _, l := range lines {
		keys = append(keys, l.Account)
	}
	return keys
}

// guardError passes err through, logging it first when a database
// guard (3.2) refused what the app had accepted: that means an app
// check is missing, and the caller sees SYS-9000.
func (s *Service) guardError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		s.log.Error("ledger: a database guard refused a request the app accepted", "error", pgErr.Message)
	}
	return err
}
