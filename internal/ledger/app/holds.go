package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// holdsReference is holds' unique (reference_type, reference_id).
const holdsReference = "holds_reference_type_reference_id_key"

var referenceTypePattern = regexp.MustCompile(`^[a-z][a-z_]*$`)

// PlaceHold reserves part of one of the tenant's wallets and posts
// nothing: tokens in a service wallet (held_tokens, against the tokens
// left in its lots; OQ-15), money in a main or bonus wallet
// (held_minor). The same reference again is a replay when it names the
// same account and amount, a conflict otherwise.
func (s *Service) PlaceHold(ctx context.Context, req HoldRequest) (HoldResult, error) {
	if err := s.validateHold(req); err != nil {
		return HoldResult{}, err
	}
	var res HoldResult
	run := func() error {
		return s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.placeHoldInTx(ctx, store.New(tx), req)
			res = r
			return err
		})
	}
	err := run()
	if uniqueViolationOn(err, holdsReference) {
		// Either a concurrent request for this hold committed first, and
		// this run is its replay, or another tenant holds the reference.
		err = run()
		if uniqueViolationOn(err, holdsReference) {
			err = fmt.Errorf("%w: hold reference already in use", ErrIdempotencyConflict)
		}
	}
	return res, s.guardError(err)
}

func (s *Service) validateHold(req HoldRequest) error {
	switch {
	case !referenceTypePattern.MatchString(req.ReferenceType) || req.ReferenceID == uuid.Nil:
		return fmt.Errorf("%w: a hold needs a reference type and ID", ErrValidation)
	case !req.ExpiresAt.After(s.clk.Now()):
		return fmt.Errorf("%w: a hold must expire in the future", ErrValidation)
	case !domain.IsClientWallet(req.Account.Type):
		return fmt.Errorf("%w: only a client's main, service or bonus wallet can be held", ErrValidation)
	}
	if err := domain.ValidateAccountKey(req.Account); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if isTokenWallet(req.Account) {
		if _, err := domain.ParseTokens(req.Tokens.String()); err != nil {
			return fmt.Errorf("%w: a service wallet hold is in tokens: %v", ErrValidation, err)
		}
		return nil
	}
	if !req.Tokens.IsZero() || req.AmountMinor <= 0 {
		return fmt.Errorf("%w: a %s hold is a positive amount of money", ErrValidation, req.Account.Type)
	}
	return nil
}

// isTokenWallet: service wallets are held in tokens (OQ-15).
func isTokenWallet(k domain.AccountKey) bool { return k.Type == domain.ServiceWallet }

func (s *Service) placeHoldInTx(ctx context.Context, q *store.Queries, req HoldRequest) (HoldResult, error) {
	existing, err := q.GetHoldByReference(ctx, store.GetHoldByReferenceParams{ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID})
	if err == nil {
		acct, err := q.GetAccount(ctx, existing.AccountID)
		if err != nil {
			return HoldResult{}, err
		}
		same := accountKey(acct) == req.Account
		if isTokenWallet(req.Account) {
			same = same && toDecimal(existing.Tokens).Equal(req.Tokens)
		} else {
			same = same && existing.AmountMinor == req.AmountMinor
		}
		if !same {
			return HoldResult{}, fmt.Errorf("%w: hold %s/%s was placed with a different account or amount",
				ErrIdempotencyConflict, req.ReferenceType, req.ReferenceID)
		}
		return HoldResult{Hold: holdView(existing), Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HoldResult{}, err
	}

	acct, err := resolveClientAccount(ctx, q, req.TenantID, req.Account)
	if err != nil {
		return HoldResult{}, err
	}
	if acct.Status != "open" {
		return HoldResult{}, fmt.Errorf("%w: the %s is %s", ErrAccountNotOpen, acct.Type, acct.Status)
	}
	locked, err := lockBalances(ctx, q, []uuid.UUID{acct.ID})
	if err != nil {
		return HoldResult{}, err
	}
	b := locked[acct.ID]

	id, err := ids.New()
	if err != nil {
		return HoldResult{}, err
	}
	params := store.CreateHoldParams{
		ID: id, TenantID: req.TenantID, AccountID: acct.ID, ReferenceType: req.ReferenceType,
		ReferenceID: req.ReferenceID, AmountMinor: req.AmountMinor, Status: string(domain.HoldActive),
		ExpiresAt: req.ExpiresAt,
	}
	if isTokenWallet(req.Account) {
		lots, err := q.ListOpenLots(ctx, acct.ID)
		if err != nil {
			return HoldResult{}, err
		}
		dl := domainLots(lots)
		held := toDecimal(b.HeldTokens)
		if available := tokensLeft(dl).Sub(held); available.LessThan(req.Tokens) {
			return HoldResult{}, fmt.Errorf("%w: %s tokens available, the hold needs %s", ErrInsufficientBalance, available, req.Tokens)
		}
		// Shown at the FIFO value of the tokens after those already held;
		// display only, at least 1 for the amount_minor CHECK (OQ-15).
		estimate, err := domain.ValueAfter(dl, held, req.Tokens)
		if err != nil {
			return HoldResult{}, err
		}
		params.AmountMinor, params.Tokens = max(estimate, 1), toNumeric(req.Tokens)
	} else if b.BalanceMinor-b.HeldMinor < req.AmountMinor {
		return HoldResult{}, fmt.Errorf("%w: %d available, the hold needs %d",
			ErrInsufficientBalance, b.BalanceMinor-b.HeldMinor, req.AmountMinor)
	}

	hold, err := q.CreateHold(ctx, params)
	if err != nil {
		return HoldResult{}, fmt.Errorf("app: writing the hold: %w", err)
	}
	if isTokenWallet(req.Account) {
		err = q.AdjustHeldTokens(ctx, store.AdjustHeldTokensParams{Delta: toNumeric(req.Tokens), AccountID: acct.ID})
	} else {
		err = q.AdjustHeld(ctx, store.AdjustHeldParams{Delta: req.AmountMinor, AccountID: acct.ID})
	}
	if err != nil {
		return HoldResult{}, fmt.Errorf("app: raising held: %w", err)
	}
	return HoldResult{Hold: holdView(hold)}, nil
}

// CaptureHold records how much of a hold was used (OQ-14): tokens for a
// token hold, money for a money hold. Without a posting the unused part
// is released and the used part stays held until settlement (3.5).
// With one (money holds only, until 3.5 builds the usage posting for
// tokens), the whole hold leaves held_minor and the posting, which must
// take exactly the captured amount from the held wallet, is made in the
// same transaction.
func (s *Service) CaptureHold(ctx context.Context, req CaptureRequest) (HoldResult, error) {
	if !referenceTypePattern.MatchString(req.ReferenceType) || req.ReferenceID == uuid.Nil {
		return HoldResult{}, fmt.Errorf("%w: a hold reference is required", ErrValidation)
	}
	if !req.CapturedTokens.IsZero() {
		if _, err := domain.ParseTokens(req.CapturedTokens.String()); err != nil {
			return HoldResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
	}
	var posting *PostRequest
	var keys []domain.AccountKey
	if req.Posting != nil {
		p := *req.Posting
		p.TenantID = &req.TenantID
		if err := validatePostRequest(p); err != nil {
			return HoldResult{}, err
		}
		posting, keys = &p, keysOf(p.Lines)
	}

	var res HoldResult
	err := s.withSystemAccounts(ctx, keys, func() error {
		return s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.captureInTx(ctx, tx, req, posting)
			res = r
			return err
		})
	})
	return res, s.guardError(err)
}

func (s *Service) captureInTx(ctx context.Context, tx pgx.Tx, req CaptureRequest, posting *PostRequest) (HoldResult, error) {
	q := store.New(tx)
	hold, acct, err := lockHold(ctx, q, req.ReferenceType, req.ReferenceID)
	if err != nil {
		return HoldResult{}, err
	}
	tokenHold := hold.Tokens.Valid
	used := req.CapturedMinor
	switch {
	case tokenHold && posting != nil:
		return HoldResult{}, fmt.Errorf("%w: a token hold's usage is posted by settlement, not with its capture", ErrValidation)
	case tokenHold && req.CapturedMinor != 0, !tokenHold && !req.CapturedTokens.IsZero():
		return HoldResult{}, fmt.Errorf("%w: capture a token hold in tokens and a money hold in money", ErrValidation)
	case tokenHold:
		used = domain.MicroTokens(req.CapturedTokens)
	}
	change, err := domain.CaptureHold(domainHold(hold), used, posting != nil)
	if errors.Is(err, domain.ErrInvalidHold) {
		return HoldResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err != nil {
		return HoldResult{}, err
	}

	if change.Replay {
		view := holdView(hold)
		if posting == nil {
			return HoldResult{Hold: view, Replayed: true}, nil
		}
		// The hold looks captured with a posting; that posting must exist
		// under this key, or this would be a plain capture settled later.
		existing, err := q.GetClientTransactionByKey(ctx, store.GetClientTransactionByKeyParams{
			TenantID: pgUUID(&req.TenantID), IdempotencyKey: posting.IdempotencyKey,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return HoldResult{}, fmt.Errorf("%w: the hold was captured without this posting", domain.ErrHoldState)
		}
		if err != nil {
			return HoldResult{}, err
		}
		r, err := replayPosting(ctx, q, existing, *posting)
		if err != nil {
			return HoldResult{}, err
		}
		view.TransactionID = &r.TransactionID
		return HoldResult{Hold: view, Replayed: true}, nil
	}

	toLock := []uuid.UUID{acct.ID}
	if posting != nil {
		deltas, err := domain.NetDeltas(posting.Lines)
		if err != nil {
			return HoldResult{}, err
		}
		if deltas[accountKey(acct)] != -req.CapturedMinor {
			return HoldResult{}, fmt.Errorf("%w: the posting must take exactly %d from the held %s, net",
				ErrValidation, req.CapturedMinor, acct.Type)
		}
		// Lock every client balance the posting touches together with the
		// held one, in one ordered statement (ADR 0010).
		accts, err := resolveAccounts(ctx, q, &req.TenantID, keysOf(posting.Lines))
		if err != nil {
			return HoldResult{}, err
		}
		for _, a := range accts {
			if a.TenantID.Valid {
				toLock = append(toLock, a.ID)
			}
		}
	}
	if _, err := lockBalances(ctx, q, toLock); err != nil {
		return HoldResult{}, err
	}
	hold, err = applyHoldChange(ctx, q, hold, change, s.clk.Now())
	if err != nil {
		return HoldResult{}, err
	}

	view := holdView(hold)
	if posting != nil {
		r, err := s.postInTx(ctx, tx, *posting)
		if err != nil {
			return HoldResult{}, err
		}
		if r.Replayed {
			// The key already belongs to a posting made before this
			// capture; un-holding without a new posting would lose the
			// charge. The transaction rolls back.
			return HoldResult{}, fmt.Errorf("%w: posting key %q is already used", ErrIdempotencyConflict, posting.IdempotencyKey)
		}
		view.TransactionID = &r.TransactionID
	}
	return HoldResult{Hold: view}, nil
}

// ReleaseHold gives an active hold's whole amount back. Releasing a
// released or expired hold is a replay.
func (s *Service) ReleaseHold(ctx context.Context, tenant uuid.UUID, referenceType string, referenceID uuid.UUID) (HoldResult, error) {
	if !referenceTypePattern.MatchString(referenceType) || referenceID == uuid.Nil {
		return HoldResult{}, fmt.Errorf("%w: a hold reference is required", ErrValidation)
	}
	var res HoldResult
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		hold, acct, err := lockHold(ctx, q, referenceType, referenceID)
		if err != nil {
			return err
		}
		change, err := domain.ReleaseHold(domainHold(hold))
		if err != nil {
			return err
		}
		if change.Replay {
			res = HoldResult{Hold: holdView(hold), Replayed: true}
			return nil
		}
		if _, err := lockBalances(ctx, q, []uuid.UUID{acct.ID}); err != nil {
			return err
		}
		hold, err = applyHoldChange(ctx, q, hold, change, s.clk.Now())
		if err != nil {
			return err
		}
		res = HoldResult{Hold: holdView(hold)}
		return nil
	})
	return res, s.guardError(err)
}

// lockHold locks the tenant's hold by reference (RLS hides another
// tenant's, which is ErrNotFound alike) and reads its account.
func lockHold(ctx context.Context, q *store.Queries, referenceType string, referenceID uuid.UUID) (store.LedgerHold, store.LedgerAccount, error) {
	hold, err := q.LockHoldByReference(ctx, store.LockHoldByReferenceParams{ReferenceType: referenceType, ReferenceID: referenceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return hold, store.LedgerAccount{}, fmt.Errorf("%w: no hold %s/%s", ErrNotFound, referenceType, referenceID)
	}
	if err != nil {
		return hold, store.LedgerAccount{}, err
	}
	acct, err := q.GetAccount(ctx, hold.AccountID)
	return hold, acct, err
}

// applyHoldChange lowers the account's held amount (held_tokens for a
// token hold, held_minor for a money hold) by what the change releases,
// records the hold's new state, and returns the hold as it now is. The
// caller holds the balance row lock.
func applyHoldChange(ctx context.Context, q *store.Queries, hold store.LedgerHold, change domain.HoldChange, now time.Time) (store.LedgerHold, error) {
	tokenHold := hold.Tokens.Valid
	if change.Release != 0 {
		var err error
		if tokenHold {
			err = q.AdjustHeldTokens(ctx, store.AdjustHeldTokensParams{Delta: toNumeric(domain.TokensFromMicro(-change.Release)), AccountID: hold.AccountID})
		} else {
			err = q.AdjustHeld(ctx, store.AdjustHeldParams{Delta: -change.Release, AccountID: hold.AccountID})
		}
		if err != nil {
			return hold, fmt.Errorf("app: lowering held: %w", err)
		}
	}
	hold.Status = string(change.Status)
	hold.CapturedMinor, hold.CapturedTokens = pgtype.Int8{}, pgtype.Numeric{}
	if change.Status == domain.HoldCaptured {
		if tokenHold {
			captured := domain.TokensFromMicro(change.Captured)
			hold.CapturedTokens = toNumeric(captured)
			// Display only: the hold's shown value pro rata, rounded up.
			shown := decimal.NewFromInt(hold.AmountMinor).Mul(captured).Div(toDecimal(hold.Tokens)).Ceil().IntPart()
			hold.CapturedMinor = pgtype.Int8{Int64: min(max(shown, 1), hold.AmountMinor), Valid: true}
		} else {
			hold.CapturedMinor = pgtype.Int8{Int64: change.Captured, Valid: true}
		}
	}
	hold.SettledAt = pgtype.Timestamptz{Time: now, Valid: change.Settle}
	err := q.UpdateHoldState(ctx, store.UpdateHoldStateParams{
		ID: hold.ID, Status: hold.Status, CapturedMinor: hold.CapturedMinor,
		CapturedTokens: hold.CapturedTokens, SettledAt: hold.SettledAt,
	})
	return hold, err
}

// domainHold reads a hold in the state machine's units: micro-tokens
// for a token hold, minor units for a money hold (OQ-15).
func domainHold(h store.LedgerHold) domain.Hold {
	if h.Tokens.Valid {
		return domain.Hold{
			Status: domain.HoldStatus(h.Status), Amount: domain.MicroTokens(toDecimal(h.Tokens)),
			Captured: domain.MicroTokens(toDecimal(h.CapturedTokens)), Settled: h.SettledAt.Valid,
		}
	}
	return domain.Hold{
		Status: domain.HoldStatus(h.Status), Amount: h.AmountMinor,
		Captured: h.CapturedMinor.Int64, Settled: h.SettledAt.Valid,
	}
}

func holdView(h store.LedgerHold) HoldView {
	return HoldView{
		ID: h.ID, AccountID: h.AccountID, ReferenceType: h.ReferenceType, ReferenceID: h.ReferenceID,
		AmountMinor: h.AmountMinor, CapturedMinor: h.CapturedMinor.Int64,
		Tokens: toDecimal(h.Tokens), CapturedTokens: toDecimal(h.CapturedTokens),
		Status: domain.HoldStatus(h.Status), Settled: h.SettledAt.Valid,
	}
}
