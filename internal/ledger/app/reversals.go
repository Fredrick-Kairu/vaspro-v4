package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// reversalReviewRole reads reversals across clients (migration
// 20261106160000).
const reversalReviewRole = "ledger_reversal_review"

// ReversalRequest is what a staff member raises (task 3.7, OQ-18): a
// reversal of part or all of one of the tenant's top-ups.
type ReversalRequest struct {
	TenantID              uuid.UUID
	OriginalTransactionID uuid.UUID
	Kind                  domain.ReversalKind
	AmountMinor           int64
	Reason                string
	EvidenceDocumentID    *uuid.UUID // accounts.documents, stored as given
	ClawbackServices      []string
}

// Reversal is a ledger.reversals row; Preview is filled by GetReversal
// while it's requested.
type Reversal struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	OriginalTransactionID uuid.UUID
	Kind                  domain.ReversalKind
	Currency              string
	AmountMinor           int64
	Reason                string
	EvidenceDocumentID    *uuid.UUID
	ClawbackServices      []string
	Status                domain.ReversalStatus
	RequestedBy           uuid.UUID
	ApprovedBy            *uuid.UUID
	RejectedBy            *uuid.UUID
	RejectionReason       string
	ReversalTransactionID *uuid.UUID
	PaybackTransactionID  *uuid.UUID
	PaybackReference      string
	PaybackAttempts       int32
	LastPaybackError      string
	CreatedAt             time.Time
	DecidedAt             *time.Time
	Preview               *ReversalPreview
}

// ReversalPreview is where a requested reversal's money would come from
// if it were approved now. Without cover, FromMainMinor is what the
// main wallet could give and no clawback is planned.
type ReversalPreview struct {
	Covered            bool
	MainAvailableMinor int64
	FromMainMinor      int64
	Clawback           []ClawbackPreview
}

// ClawbackPreview is one named wallet in a preview: what it could give
// (ClawableMinor), and what the reversal would take (Tokens,
// ValueMinor).
type ClawbackPreview struct {
	ServiceCode   string
	ClawableMinor int64
	Tokens        decimal.Decimal
	ValueMinor    int64
}

// ReversalFilter narrows staff's list.
type ReversalFilter struct {
	TenantID *uuid.UUID
	Status   string
	Cursor   string
	Limit    int
}

// ReversalPage is one page of staff's list, oldest first.
type ReversalPage struct {
	Reversals  []Reversal
	NextCursor string
}

// PaybackReport is the rail's outcome for a pay-back (Payments reports
// it from Phase 13).
type PaybackReport struct {
	TenantID   uuid.UUID
	ReversalID uuid.UUID
	Succeeded  bool
	Reference  string
	Error      string
}

// topUp is what a reversal needs from the transaction it reverses.
type topUp struct {
	id       uuid.UUID
	amount   int64             // what it credited the main wallet
	currency string            // the main wallet's
	clearing domain.AccountKey // the rail's provider_clearing it debited
}

// RequestReversal is the maker's step (system design §4.3, OQ-18): a
// staff member with a fresh step-up raises a reversal of the tenant's
// top-up. Nothing is posted until another staff member approves it.
// Under the main wallet's balance lock, the live reversals of the
// top-up plus this one must not exceed it (VAL-4015).
func (s *Service) RequestReversal(ctx context.Context, staffID, sessionID uuid.UUID, req ReversalRequest) (Reversal, error) {
	if err := s.requireStaffStepUp(ctx, staffID, sessionID); err != nil {
		return Reversal{}, err
	}
	id, err := ids.New()
	if err != nil {
		return Reversal{}, err
	}
	var row store.LedgerReversal
	err = s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		original, err := readTopUp(ctx, q, req.OriginalTransactionID)
		if err != nil {
			return err
		}
		if err := domain.ValidateNewReversal(domain.NewReversal{
			Kind: req.Kind, Currency: original.currency, AmountMinor: req.AmountMinor,
			Reason: req.Reason, ClawbackServices: req.ClawbackServices,
		}); err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}
		if err := lockMainAndCheckCap(ctx, q, req.TenantID, original, req.AmountMinor, 0); err != nil {
			return err
		}
		row, err = q.CreateReversal(ctx, store.CreateReversalParams{
			ID: id, TenantID: req.TenantID, OriginalTransactionID: original.id, AmountMinor: req.AmountMinor,
			Reason: strings.TrimSpace(req.Reason), EvidenceDocumentID: pgUUID(req.EvidenceDocumentID),
			Status: string(domain.ReversalRequested), RequestedBy: staffID, Kind: string(req.Kind),
			Currency: original.currency, ClawbackServices: append([]string{}, req.ClawbackServices...),
		})
		if err != nil {
			return fmt.Errorf("app: writing the reversal: %w", err)
		}
		data := reversalEventData(row)
		data["reason"], data["requested_by"], data["clawback_services"] = row.Reason, row.RequestedBy, row.ClawbackServices
		if row.EvidenceDocumentID.Valid {
			data["evidence_document_id"] = uuid.UUID(row.EvidenceDocumentID.Bytes)
		}
		return s.writeReversalEvent(ctx, tx, "reversal.requested", row, data)
	})
	if err != nil {
		return Reversal{}, err
	}
	return reversalView(row), nil
}

// ApproveReversal is the checker's step: another staff member than the
// maker, with a fresh step-up, and a finance lead at or above the
// pack's threshold, approves; the reversal is posted in the same
// transaction. The main wallet gives what it has available, then the
// named service wallets their unused, unheld tokens, newest lot first
// (domain.PlanReversal). If that can't cover it, nothing is written
// (BAL-4007) and it stays requested.
func (s *Service) ApproveReversal(ctx context.Context, staffID, sessionID, reversalID uuid.UUID) (Reversal, error) {
	found, err := s.checkDecider(ctx, staffID, sessionID, reversalID)
	if err != nil {
		return Reversal{}, err
	}
	needsLead := domain.NeedsFinanceLead(found.AmountMinor, s.financeLeadThreshold(found.Currency))
	if needsLead {
		lead, err := s.identity.IsFinanceLead(ctx, staffID)
		if err != nil {
			return Reversal{}, fmt.Errorf("app: checking finance lead: %w", err)
		}
		if !lead {
			return Reversal{}, ErrFinanceLeadRequired
		}
	}

	var row store.LedgerReversal
	vaspro := []domain.AccountKey{{Type: domain.ReversalClearing, Currency: found.Currency}}
	err = s.withSystemAccounts(ctx, vaspro, func() error {
		return s.pool.WithTenantTx(ctx, found.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = s.approveInTx(ctx, tx, staffID, reversalID, needsLead)
			return err
		})
	})
	if err != nil {
		return Reversal{}, err
	}
	return reversalView(row), nil
}

func (s *Service) approveInTx(ctx context.Context, tx pgx.Tx, staffID, reversalID uuid.UUID, needsLead bool) (store.LedgerReversal, error) {
	q := store.New(tx)
	rev, err := lockReversal(ctx, q, reversalID)
	if err != nil {
		return rev, err
	}
	if err := domain.CanDecideReversal(domain.ReversalStatus(rev.Status)); err != nil {
		return rev, err
	}
	original, err := readTopUp(ctx, q, rev.OriginalTransactionID)
	if err != nil {
		return rev, err
	}
	// The cap was checked at request, under the same lock; checked again
	// here so approving can never take the top-up past what it paid.
	if err := lockMainAndCheckCap(ctx, q, rev.TenantID, original, rev.AmountMinor, rev.AmountMinor); err != nil {
		return rev, err
	}

	plan, mainKey, wallets, err := planReversal(ctx, q, rev, true)
	if err != nil {
		return rev, err
	}
	kind := domain.ReversalKind(rev.Kind)
	credit := domain.AccountKey{Type: domain.ReversalClearing, Currency: rev.Currency}
	if kind != domain.ReversalPayback {
		credit = original.clearing // the rail already moved the money back
	}
	lines := []domain.Line{{Account: credit, Direction: domain.Credit, AmountMinor: rev.AmountMinor}}
	switch {
	case plan.MainMinor > 0:
		lines = append(lines, domain.Line{Account: mainKey, Direction: domain.Debit, AmountMinor: plan.MainMinor})
	case plan.MainMinor < 0:
		lines = append(lines, domain.Line{Account: mainKey, Direction: domain.Credit, AmountMinor: -plan.MainMinor})
	}
	var draws []domain.Draw
	clawback := make([]map[string]any, 0, len(plan.Wallets))
	for _, w := range plan.Wallets {
		lines = append(lines, domain.Line{Account: wallets[w.ServiceCode], Direction: domain.Debit, AmountMinor: w.ValueMinor, Tokens: w.Tokens.String()})
		draws = append(draws, w.Draws...)
		clawback = append(clawback, map[string]any{"service_code": w.ServiceCode, "tokens": w.Tokens.String(), "value_minor": w.ValueMinor})
	}
	posted, err := s.postInTx(ctx, tx, PostRequest{
		TenantID: &rev.TenantID, IdempotencyKey: "reversal:" + rev.ID.String(), Type: domain.TxnReversal,
		Description:   fmt.Sprintf("%s reversal of top-up %s: %s", rev.Kind, rev.OriginalTransactionID, rev.Reason),
		ReferenceType: "reversal", ReferenceID: &rev.ID, CreatedBy: &staffID, Lines: lines,
		internal: true, reverses: &rev.OriginalTransactionID,
	})
	if err != nil {
		return rev, err
	}
	for _, d := range draws {
		if err := q.DrawFromLot(ctx, store.DrawFromLotParams{ID: d.LotID, Tokens: toNumeric(d.Tokens)}); err != nil {
			return rev, fmt.Errorf("app: clawing back from a lot: %w", err)
		}
	}
	if err := recordDraws(ctx, q, draws, posted.TransactionID); err != nil {
		return rev, err
	}

	status := domain.StatusAfterPosting(kind)
	approved, err := q.ApproveReversal(ctx, store.ApproveReversalParams{
		ID: rev.ID, Status: string(status), ApprovedBy: pgUUID(&staffID),
		DecidedAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true}, ReversalTransactionID: pgUUID(&posted.TransactionID),
	})
	if err != nil {
		return rev, fmt.Errorf("app: approving the reversal: %w", err)
	}
	data := reversalEventData(approved)
	data["requested_by"], data["approved_by"], data["finance_lead_required"] = approved.RequestedBy, staffID, needsLead
	if err := s.writeReversalEvent(ctx, tx, "reversal.approved", approved, data); err != nil {
		return rev, err
	}
	data = reversalEventData(approved)
	data["transaction_id"], data["status"], data["from_main_minor"], data["clawback"] = posted.TransactionID, string(status), plan.MainMinor, clawback
	return approved, s.writeReversalEvent(ctx, tx, "reversal.posted", approved, data)
}

// RejectReversal: another staff member than the maker, with a fresh
// step-up, rejects a requested reversal, giving a reason. Nothing is
// posted, and its share of the top-up is free again.
func (s *Service) RejectReversal(ctx context.Context, staffID, sessionID, reversalID uuid.UUID, reason string) (Reversal, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Reversal{}, fmt.Errorf("%w: a rejection needs a reason", ErrValidation)
	}
	found, err := s.checkDecider(ctx, staffID, sessionID, reversalID)
	if err != nil {
		return Reversal{}, err
	}
	var row store.LedgerReversal
	err = s.pool.WithTenantTx(ctx, found.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rev, err := lockReversal(ctx, q, reversalID)
		if err != nil {
			return err
		}
		if err := domain.CanDecideReversal(domain.ReversalStatus(rev.Status)); err != nil {
			return err
		}
		row, err = q.RejectReversal(ctx, store.RejectReversalParams{
			ID: rev.ID, RejectedBy: pgUUID(&staffID), RejectionReason: text(reason),
			DecidedAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("app: rejecting the reversal: %w", err)
		}
		data := reversalEventData(row)
		data["requested_by"], data["rejected_by"], data["reason"] = row.RequestedBy, staffID, reason
		return s.writeReversalEvent(ctx, tx, "reversal.rejected", row, data)
	})
	if err != nil {
		return Reversal{}, err
	}
	return reversalView(row), nil
}

// checkDecider is the checks before a decision: staff with a fresh
// step-up, a reversal that exists and is still requested, and not its
// maker (maker-checker; the database's CHECK backs it).
func (s *Service) checkDecider(ctx context.Context, staffID, sessionID, reversalID uuid.UUID) (store.LedgerReversal, error) {
	if err := s.requireStaffStepUp(ctx, staffID, sessionID); err != nil {
		return store.LedgerReversal{}, err
	}
	found, err := s.findReversal(ctx, reversalID)
	if err != nil {
		return found, err
	}
	if err := domain.CanDecideReversal(domain.ReversalStatus(found.Status)); err != nil {
		return found, err
	}
	if found.RequestedBy == staffID {
		return found, ErrMakerCannotApprove
	}
	return found, nil
}

// GetReversal is one reversal for staff; while it's requested it
// carries a live preview of where its money would come from.
func (s *Service) GetReversal(ctx context.Context, staffID, reversalID uuid.UUID) (Reversal, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return Reversal{}, err
	}
	found, err := s.findReversal(ctx, reversalID)
	if err != nil {
		return Reversal{}, err
	}
	out := reversalView(found)
	if found.Status != string(domain.ReversalRequested) {
		return out, nil
	}
	err = s.pool.WithTenantTx(ctx, found.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		out.Preview, err = previewReversal(ctx, store.New(tx), found)
		return err
	})
	if err != nil {
		return Reversal{}, err
	}
	return out, nil
}

// ListReversals is staff's queue across clients, oldest first, read as
// ledger_reversal_review.
func (s *Service) ListReversals(ctx context.Context, staffID uuid.UUID, f ReversalFilter) (ReversalPage, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return ReversalPage{}, err
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultPricePage
	}
	if limit < 1 || limit > maxPricePage {
		return ReversalPage{}, fmt.Errorf("%w: limit is 1 to %d", ErrValidation, maxPricePage)
	}
	params := store.ListReversalsPageParams{TenantID: pgUUID(f.TenantID), Status: text(f.Status), MaxRows: int32(limit + 1)} //nolint:gosec // limit is at most maxPricePage
	if f.Cursor != "" {
		after, err := decodePriceCursor(f.Cursor)
		if err != nil {
			return ReversalPage{}, err
		}
		params.After = pgUUID(&after)
	}
	var rows []store.LedgerReversal
	err := s.pool.WithRoleTx(ctx, reversalReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListReversalsPage(ctx, params)
		return err
	})
	if err != nil {
		return ReversalPage{}, fmt.Errorf("app: listing reversals: %w", err)
	}
	var page ReversalPage
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString(rows[limit-1].ID[:])
	}
	for _, r := range rows {
		page.Reversals = append(page.Reversals, reversalView(r))
	}
	return page, nil
}

// RecordPayback applies the rail's report on a pay-back (OQ-18;
// Payments calls it over gRPC from Phase 13). Success posts the
// pay-back, from reversal clearing to the rail's provider clearing; a
// repeat of it is a replay. A failure is recorded and the money stays
// in clearing for the retry.
func (s *Service) RecordPayback(ctx context.Context, r PaybackReport) (Reversal, error) {
	var row store.LedgerReversal
	// The reversal's currency names the clearing account to open, if no
	// approval has opened it yet.
	err := s.pool.WithTenantTx(ctx, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetReversal(ctx, r.ReversalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no reversal %s", ErrNotFound, r.ReversalID)
		}
		return err
	})
	if err != nil {
		return Reversal{}, err
	}
	clearing := []domain.AccountKey{{Type: domain.ReversalClearing, Currency: row.Currency}}
	err = s.withSystemAccounts(ctx, clearing, func() error {
		return s.pool.WithTenantTx(ctx, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = s.recordPaybackInTx(ctx, tx, r)
			return err
		})
	})
	if err != nil {
		return Reversal{}, err
	}
	return reversalView(row), nil
}

func (s *Service) recordPaybackInTx(ctx context.Context, tx pgx.Tx, r PaybackReport) (store.LedgerReversal, error) {
	q := store.New(tx)
	rev, err := lockReversal(ctx, q, r.ReversalID)
	if err != nil {
		return rev, err
	}
	change, err := domain.RecordPayback(domain.ReversalStatus(rev.Status), r.Succeeded)
	switch {
	case err != nil:
		return rev, err
	case change.Replay:
		return rev, nil
	case change.Attempt:
		return q.RecordReversalPaybackFailure(ctx, store.RecordReversalPaybackFailureParams{
			ID: rev.ID, LastPaybackError: text(strings.TrimSpace(r.Error)), PaybackReference: text(r.Reference),
		})
	}
	original, err := readTopUp(ctx, q, rev.OriginalTransactionID)
	if err != nil {
		return rev, err
	}
	clearing := domain.AccountKey{Type: domain.ReversalClearing, Currency: rev.Currency}
	posted, err := s.postInTx(ctx, tx, PostRequest{
		TenantID: &rev.TenantID, IdempotencyKey: "payback:" + rev.ID.String(), Type: domain.TxnPayback,
		Description:   fmt.Sprintf("pay-back of reversal %s", rev.ID),
		ReferenceType: "reversal", ReferenceID: &rev.ID,
		Lines: []domain.Line{
			{Account: clearing, Direction: domain.Debit, AmountMinor: rev.AmountMinor},
			{Account: original.clearing, Direction: domain.Credit, AmountMinor: rev.AmountMinor},
		},
		internal: true, reverses: &rev.OriginalTransactionID,
	})
	if err != nil {
		return rev, err
	}
	row, err := q.MarkReversalPaidBack(ctx, store.MarkReversalPaidBackParams{
		ID: rev.ID, PaybackTransactionID: pgUUID(&posted.TransactionID), PaybackReference: text(r.Reference),
	})
	if err != nil {
		return rev, fmt.Errorf("app: marking the pay-back: %w", err)
	}
	data := reversalEventData(row)
	data["payback_transaction_id"], data["attempts"] = posted.TransactionID, row.PaybackAttempts
	if r.Reference != "" {
		data["payback_reference"] = r.Reference
	}
	return row, s.writeReversalEvent(ctx, tx, "reversal.paid_back", row, data)
}

// readTopUp reads the tenant's top-up a reversal undoes. Anything else,
// or another tenant's (hidden by RLS), is not found.
func readTopUp(ctx context.Context, q *store.Queries, id uuid.UUID) (topUp, error) {
	txn, err := q.GetTransaction(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (domain.TxnType(txn.Type) != domain.TxnTopup || !txn.TenantID.Valid)) {
		return topUp{}, fmt.Errorf("%w: no top-up %s", ErrNotFound, id)
	}
	if err != nil {
		return topUp{}, fmt.Errorf("app: reading the top-up: %w", err)
	}
	lines, err := q.ListPostedLines(ctx, id)
	if err != nil {
		return topUp{}, fmt.Errorf("app: reading the top-up's lines: %w", err)
	}
	out := topUp{id: id}
	var hasClearing bool
	for _, l := range lines {
		key := keyOf(l.Type, l.ServiceCode, l.Rail, l.Currency)
		switch {
		case key.Type == domain.MainWallet && l.Direction == "C":
			out.amount += l.AmountMinor
			out.currency = key.Currency
		case key.Type == domain.ProviderClearing && l.Direction == "D":
			out.clearing, hasClearing = key, true
		}
	}
	if out.amount == 0 || !hasClearing {
		return topUp{}, fmt.Errorf("%w: top-up %s has no main-wallet credit from a rail to reverse", ErrValidation, id)
	}
	return out, nil
}

// lockMainAndCheckCap locks the tenant's main wallet balance (which
// serialises reversals of its top-ups: the service can't lock the
// append-only transaction) and checks the top-up's live reversals plus
// amount, less self (a reversal already counted), fit in it.
func lockMainAndCheckCap(ctx context.Context, q *store.Queries, tenant uuid.UUID, original topUp, amount, self int64) error {
	main, err := resolveClientAccount(ctx, q, tenant, domain.AccountKey{Type: domain.MainWallet, Currency: original.currency})
	if err != nil {
		return err
	}
	if _, err := lockBalances(ctx, q, []uuid.UUID{main.ID}); err != nil {
		return err
	}
	live, err := q.SumLiveReversals(ctx, original.id)
	if err != nil {
		return fmt.Errorf("app: summing reversals: %w", err)
	}
	return domain.CheckReversalCap(original.amount, live-self, amount)
}

// planReversal reads (lock: under FOR UPDATE, in balance then lot
// order) the main wallet and the named wallets and plans the reversal.
// A named wallet the client never opened has nothing to give.
func planReversal(ctx context.Context, q *store.Queries, rev store.LedgerReversal, lock bool) (domain.ReversalPlan, domain.AccountKey, map[string]domain.AccountKey, error) {
	mainKey := domain.AccountKey{Type: domain.MainWallet, Currency: rev.Currency}
	main, err := resolveClientAccount(ctx, q, rev.TenantID, mainKey)
	if err != nil {
		return domain.ReversalPlan{}, mainKey, nil, err
	}
	keys := map[string]domain.AccountKey{}
	var accts []store.LedgerAccount
	for _, code := range rev.ClawbackServices {
		k := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: code, Currency: rev.Currency}
		a, err := q.FindClientAccount(ctx, store.FindClientAccountParams{
			TenantID: pgUUID(&rev.TenantID), Type: store.LedgerAccountType(k.Type), Currency: k.Currency, ServiceCode: text(code),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return domain.ReversalPlan{}, mainKey, nil, err
		}
		keys[code] = k
		accts = append(accts, a)
	}
	balances, err := readBalances(ctx, q, append([]store.LedgerAccount{main}, accts...), lock)
	if err != nil {
		return domain.ReversalPlan{}, mainKey, nil, err
	}
	wallets := make([]domain.ClawbackWallet, 0, len(accts))
	for _, a := range accts {
		var lots []store.LedgerTokenLot
		if lock {
			lots, err = q.LockOpenLots(ctx, a.ID)
		} else {
			lots, err = q.ListOpenLots(ctx, a.ID)
		}
		if err != nil {
			return domain.ReversalPlan{}, mainKey, nil, fmt.Errorf("app: reading lots: %w", err)
		}
		wallets = append(wallets, domain.ClawbackWallet{ServiceCode: a.ServiceCode.String, Lots: domainLots(lots), Held: balances[a.ID].held})
	}
	mainAvailable := balances[main.ID].available
	plan, err := domain.PlanReversal(rev.AmountMinor, mainAvailable, wallets)
	if err != nil {
		return domain.ReversalPlan{MainMinor: min(max(mainAvailable, 0), rev.AmountMinor)}, mainKey, keys, err
	}
	return plan, mainKey, keys, nil
}

type balanceRead struct {
	available int64
	held      decimal.Decimal
}

func readBalances(ctx context.Context, q *store.Queries, accts []store.LedgerAccount, lock bool) (map[uuid.UUID]balanceRead, error) {
	out := make(map[uuid.UUID]balanceRead, len(accts))
	if lock {
		ids := make([]uuid.UUID, len(accts))
		for i, a := range accts {
			ids[i] = a.ID
		}
		locked, err := lockBalances(ctx, q, ids)
		if err != nil {
			return nil, err
		}
		for id, b := range locked {
			out[id] = balanceRead{available: b.BalanceMinor - b.HeldMinor, held: toDecimal(b.HeldTokens)}
		}
		return out, nil
	}
	for _, a := range accts {
		b, err := q.GetBalance(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("app: reading a balance: %w", err)
		}
		out[a.ID] = balanceRead{available: b.BalanceMinor - b.HeldMinor, held: toDecimal(b.HeldTokens)}
	}
	return out, nil
}

// previewReversal plans a requested reversal without locking anything:
// what staff see before they approve.
func previewReversal(ctx context.Context, q *store.Queries, rev store.LedgerReversal) (*ReversalPreview, error) {
	plan, _, _, err := planReversal(ctx, q, rev, false)
	covered := err == nil
	if err != nil && !errors.Is(err, domain.ErrReversalNotCovered) {
		return nil, err
	}
	p := &ReversalPreview{Covered: covered, FromMainMinor: plan.MainMinor}
	mainKey := domain.AccountKey{Type: domain.MainWallet, Currency: rev.Currency}
	main, err := resolveClientAccount(ctx, q, rev.TenantID, mainKey)
	if err != nil {
		return nil, err
	}
	b, err := q.GetBalance(ctx, main.ID)
	if err != nil {
		return nil, err
	}
	p.MainAvailableMinor = b.BalanceMinor - b.HeldMinor
	taken := map[string]domain.WalletClawback{}
	for _, w := range plan.Wallets {
		taken[w.ServiceCode] = w
	}
	for _, code := range rev.ClawbackServices {
		cp := ClawbackPreview{ServiceCode: code, Tokens: decimal.Zero}
		a, err := q.FindClientAccount(ctx, store.FindClientAccountParams{
			TenantID: pgUUID(&rev.TenantID), Type: store.LedgerAccountType(domain.ServiceWallet), Currency: rev.Currency, ServiceCode: text(code),
		})
		if err == nil {
			wb, err := q.GetBalance(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			lots, err := q.ListOpenLots(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			if cp.ClawableMinor, err = domain.ClawableValue(domain.ClawbackWallet{ServiceCode: code, Lots: domainLots(lots), Held: toDecimal(wb.HeldTokens)}); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if covered {
			cp.Tokens, cp.ValueMinor = taken[code].Tokens, taken[code].ValueMinor
			if cp.Tokens.IsZero() {
				cp.Tokens = decimal.Zero
			}
		}
		p.Clawback = append(p.Clawback, cp)
	}
	return p, nil
}

// findReversal finds a reversal across clients, as
// ledger_reversal_review, so staff can then act in its tenant's own
// transaction.
func (s *Service) findReversal(ctx context.Context, id uuid.UUID) (store.LedgerReversal, error) {
	var row store.LedgerReversal
	err := s.pool.WithRoleTx(ctx, reversalReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetReversal(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("%w: no reversal %s", ErrNotFound, id)
	}
	if err != nil {
		return row, fmt.Errorf("app: finding the reversal: %w", err)
	}
	return row, nil
}

func lockReversal(ctx context.Context, q *store.Queries, id uuid.UUID) (store.LedgerReversal, error) {
	row, err := q.LockReversal(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("%w: no reversal %s", ErrNotFound, id)
	}
	if err != nil {
		return row, fmt.Errorf("app: locking the reversal: %w", err)
	}
	return row, nil
}

// financeLeadThreshold is the currency's pack threshold; with no pack,
// or none in it, nil: every reversal needs a finance lead.
func (s *Service) financeLeadThreshold(currency string) *int64 {
	p, err := s.packForCurrency(currency)
	if err != nil {
		return nil
	}
	return p.Ledger.ReversalFinanceLeadMinor
}

func reversalEventData(r store.LedgerReversal) map[string]any {
	return map[string]any{
		"tenant_id": r.TenantID, "reversal_id": r.ID, "original_transaction_id": r.OriginalTransactionID,
		"kind": r.Kind, "currency": r.Currency, "amount_minor": r.AmountMinor,
	}
}

func (s *Service) writeReversalEvent(ctx context.Context, tx pgx.Tx, eventType string, r store.LedgerReversal, data map[string]any) error {
	eventID, err := ids.New()
	if err != nil {
		return err
	}
	env, err := platformevents.New(eventID, eventType, 1, r.TenantID, s.clk.Now(), "", data)
	if err != nil {
		return fmt.Errorf("app: building %s: %w", eventType, err)
	}
	return outbox.Write(ctx, tx, "reversal", r.ID, env)
}

func reversalView(r store.LedgerReversal) Reversal {
	return Reversal{
		ID: r.ID, TenantID: r.TenantID, OriginalTransactionID: r.OriginalTransactionID,
		Kind: domain.ReversalKind(r.Kind), Currency: r.Currency, AmountMinor: r.AmountMinor, Reason: r.Reason,
		EvidenceDocumentID: optionalID(r.EvidenceDocumentID), ClawbackServices: r.ClawbackServices,
		Status: domain.ReversalStatus(r.Status), RequestedBy: r.RequestedBy,
		ApprovedBy: optionalID(r.ApprovedBy), RejectedBy: optionalID(r.RejectedBy), RejectionReason: r.RejectionReason.String,
		ReversalTransactionID: optionalID(r.ReversalTransactionID), PaybackTransactionID: optionalID(r.PaybackTransactionID),
		PaybackReference: r.PaybackReference.String, PaybackAttempts: r.PaybackAttempts,
		LastPaybackError: r.LastPaybackError.String, CreatedAt: r.CreatedAt, DecidedAt: optTime(r.DecidedAt),
	}
}

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	at := t.Time
	return &at
}
