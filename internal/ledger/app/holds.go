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

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// holdsReference is holds' unique (reference_type, reference_id).
const holdsReference = "holds_reference_type_reference_id_key"

var referenceTypePattern = regexp.MustCompile(`^[a-z][a-z_]*$`)

// PlaceHold reserves money in one of the tenant's wallets: it raises
// held_minor and posts nothing. The same reference again is a replay
// when it names the same account and amount, a conflict otherwise.
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
	case req.AmountMinor <= 0:
		return fmt.Errorf("%w: a hold must be positive", ErrValidation)
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
	return nil
}

func (s *Service) placeHoldInTx(ctx context.Context, q *store.Queries, req HoldRequest) (HoldResult, error) {
	existing, err := q.GetHoldByReference(ctx, store.GetHoldByReferenceParams{ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID})
	if err == nil {
		acct, err := q.GetAccount(ctx, existing.AccountID)
		if err != nil {
			return HoldResult{}, err
		}
		if accountKey(acct) != req.Account || existing.AmountMinor != req.AmountMinor {
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
	if b := locked[acct.ID]; b.BalanceMinor-b.HeldMinor < req.AmountMinor {
		return HoldResult{}, fmt.Errorf("%w: %d available, the hold needs %d",
			ErrInsufficientBalance, b.BalanceMinor-b.HeldMinor, req.AmountMinor)
	}

	id, err := ids.New()
	if err != nil {
		return HoldResult{}, err
	}
	hold, err := q.CreateHold(ctx, store.CreateHoldParams{
		ID: id, TenantID: req.TenantID, AccountID: acct.ID, ReferenceType: req.ReferenceType,
		ReferenceID: req.ReferenceID, AmountMinor: req.AmountMinor, Status: string(domain.HoldActive),
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		return HoldResult{}, fmt.Errorf("app: writing the hold: %w", err)
	}
	if err := q.AdjustHeld(ctx, store.AdjustHeldParams{Delta: req.AmountMinor, AccountID: acct.ID}); err != nil {
		return HoldResult{}, fmt.Errorf("app: raising held: %w", err)
	}
	return HoldResult{Hold: holdView(hold)}, nil
}

// CaptureHold records how much of a hold was used (OQ-14). Without a
// posting the unused part is released and the used part stays held
// until settlement (3.5). With one, the whole hold leaves held_minor
// and the posting, which must take exactly the captured amount from the
// held wallet, is made in the same transaction.
func (s *Service) CaptureHold(ctx context.Context, req CaptureRequest) (HoldResult, error) {
	if !referenceTypePattern.MatchString(req.ReferenceType) || req.ReferenceID == uuid.Nil {
		return HoldResult{}, fmt.Errorf("%w: a hold reference is required", ErrValidation)
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
	change, err := domain.CaptureHold(domainHold(hold), req.CapturedMinor, posting != nil)
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
	if err := applyHoldChange(ctx, q, hold, change, s.clk.Now()); err != nil {
		return HoldResult{}, err
	}

	hold.Status = string(change.Status)
	hold.CapturedMinor = pgtype.Int8{Int64: change.CapturedMinor, Valid: true}
	hold.SettledAt = pgtype.Timestamptz{Time: s.clk.Now(), Valid: change.Settle}
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
		if err := applyHoldChange(ctx, q, hold, change, s.clk.Now()); err != nil {
			return err
		}
		hold.Status = string(change.Status)
		hold.SettledAt = pgtype.Timestamptz{Time: s.clk.Now(), Valid: true}
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

// applyHoldChange lowers held_minor by what the change releases and
// records the hold's new state. The caller holds the balance row lock.
func applyHoldChange(ctx context.Context, q *store.Queries, hold store.LedgerHold, change domain.HoldChange, now time.Time) error {
	if change.ReleaseMinor != 0 {
		if err := q.AdjustHeld(ctx, store.AdjustHeldParams{Delta: -change.ReleaseMinor, AccountID: hold.AccountID}); err != nil {
			return fmt.Errorf("app: lowering held: %w", err)
		}
	}
	captured := pgtype.Int8{}
	if change.Status == domain.HoldCaptured {
		captured = pgtype.Int8{Int64: change.CapturedMinor, Valid: true}
	}
	return q.UpdateHoldState(ctx, store.UpdateHoldStateParams{
		ID: hold.ID, Status: string(change.Status), CapturedMinor: captured,
		SettledAt: pgtype.Timestamptz{Time: now, Valid: change.Settle},
	})
}

func domainHold(h store.LedgerHold) domain.Hold {
	return domain.Hold{
		Status: domain.HoldStatus(h.Status), AmountMinor: h.AmountMinor,
		CapturedMinor: h.CapturedMinor.Int64, Settled: h.SettledAt.Valid,
	}
}

func holdView(h store.LedgerHold) HoldView {
	return HoldView{
		ID: h.ID, AccountID: h.AccountID, ReferenceType: h.ReferenceType, ReferenceID: h.ReferenceID,
		AmountMinor: h.AmountMinor, CapturedMinor: h.CapturedMinor.Int64,
		Status: domain.HoldStatus(h.Status), Settled: h.SettledAt.Valid,
	}
}
