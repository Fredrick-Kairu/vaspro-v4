package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"
	_ "time/tzdata" // the packs' time zones, wherever the binary runs

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

var (
	// ErrBadCallback: a callback from outside the rail's published
	// ranges, or without the right path token (AUTH-4013). Nothing is
	// recorded.
	ErrBadCallback = errors.New("app: callback not from the rail")
	// ErrIdempotencyConflict: an Idempotency-Key reused for a different
	// top-up (VAL-4001).
	ErrIdempotencyConflict = errors.New("app: key reused with a different request")
)

// sweepBatch bounds each step of one sweep.
const sweepBatch = 100

// Topup is an STK top-up as the dashboard sees it.
type Topup struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	Status           string
	AmountMinor      int64
	Currency         string
	PayerPhone       string
	AccountReference string
	Receipt          string
	ErrorCode        string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	CompletedAt      *time.Time
}

func topupOf(r store.PaymentsPaymentIntent) Topup {
	t := Topup{
		ID: r.ID, TenantID: r.TenantID, Status: string(r.Status), AmountMinor: r.AmountMinor, Currency: r.Currency,
		PayerPhone: r.PayerPhoneE164.String, AccountReference: r.AccountReference, Receipt: r.RailReceipt.String,
		ErrorCode: r.ErrorCode.String, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
	}
	if r.CompletedAt.Valid {
		at := r.CompletedAt.Time
		t.CompletedAt = &at
	}
	return t
}

// StartTopup prompts the payer's phone to pay VASPRO's paybill for the
// tenant's country (task 4.4). The intent is recorded before the push,
// so a callback that beats the push's response finds it; a replayed
// idempotency key returns the same intent and never prompts again. An
// outcome the rail gives at once is returned as a failed top-up; a
// number the rail adapter refuses is ErrValidation as well.
func (s *Service) StartTopup(ctx context.Context, user, tenant uuid.UUID, idempotencyKey string, amountMinor int64, phone string) (Topup, error) {
	info, err := s.requireTenant(ctx, user, tenant, PermBillingManage)
	if err != nil {
		return Topup{}, err
	}
	return s.startTopup(ctx, tenant, info.CountryCode, info.Currency, idempotencyKey, amountMinor, phone, nil)
}

// startTopup is StartTopup once the tenant is known: from the dashboard,
// or from a checkout link (task 4.8), whose payments carry its link ID.
func (s *Service) startTopup(ctx context.Context, tenant uuid.UUID, country, currency, idempotencyKey string, amountMinor int64, phone string, link *uuid.UUID) (Topup, error) {
	if idempotencyKey == "" {
		return Topup{}, fmt.Errorf("%w: an idempotency key is required", ErrValidation)
	}
	pack, ok := s.packs[country]
	if !ok {
		return Topup{}, fmt.Errorf("%w: no pack for %s", ErrTopupUnavailable, country)
	}
	rail, ok := pack.TopupRail()
	if !ok {
		return Topup{}, fmt.Errorf("%w: %s has no top-up rail", ErrTopupUnavailable, country)
	}
	rules := domain.TopupRules{CurrencyExponent: pack.Currency.Exponent, CountryCode: pack.Numbering.CountryCode, NationalNumberLength: pack.Numbering.NationalSignificantNumberLength}
	if err := domain.ValidateTopup(amountMinor, phone, rules); err != nil {
		return Topup{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	client, ok := s.rails[rail.Code]
	if !ok {
		return Topup{}, fmt.Errorf("%w: no client for rail %s", ErrTopupUnavailable, rail.Code)
	}

	var (
		intent   store.PaymentsPaymentIntent
		replayed bool
	)
	linkID := pgtype.UUID{}
	if link != nil {
		linkID = pgtype.UUID{Bytes: *link, Valid: true}
	}
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		prev, err := q.GetPaymentIntentByIdempotencyKey(ctx, store.GetPaymentIntentByIdempotencyKeyParams{TenantID: tenant, IdempotencyKey: idempotencyKey})
		if err == nil {
			if prev.AmountMinor != amountMinor || prev.PayerPhoneE164.String != phone {
				return fmt.Errorf("%w: key %q", ErrIdempotencyConflict, idempotencyKey)
			}
			intent, replayed = prev, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: loading the top-up: %w", err)
		}
		paybill, err := q.GetActiveTopupRailAccount(ctx, rail.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no verified paybill on %s", ErrTopupUnavailable, rail.Code)
		}
		if err != nil {
			return fmt.Errorf("app: loading VASPRO's paybill: %w", err)
		}
		number, err := s.ensureTopupAccount(ctx, q, tenant, paybill.ID)
		if err != nil {
			return err
		}
		id, err := ids.New()
		if err != nil {
			return err
		}
		now := s.clk.Now()
		intent, err = q.CreatePaymentIntent(ctx, store.CreatePaymentIntentParams{
			ID: id, TenantID: tenant, Purpose: store.PaymentsIntentPurposeWalletTopup, RailAccountID: paybill.ID,
			Method: "stk_push", AmountMinor: amountMinor, Currency: currency,
			PayerPhoneE164: pgtype.Text{String: phone, Valid: true}, AccountReference: number,
			PaymentLinkID: linkID, IdempotencyKey: idempotencyKey, ExpiresAt: now.Add(s.expiry), CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("app: recording the top-up: %w", err)
		}
		return nil
	})
	if err != nil || replayed {
		return topupOf(intent), err
	}

	resp, err := client.STKPush(ctx, adapters.STKPushRequest{
		RailAccountID: intent.RailAccountID, AmountMinor: amountMinor, PhoneE164: phone,
		AccountReference: intent.AccountReference, Description: "Top-up",
		CallbackURL: s.stkCallbackURL(intent.RailAccountID),
	})
	if err != nil {
		code := adapters.ErrorCodeFor(err)
		s.log.WarnContext(ctx, "payments: STK push failed", "intent_id", intent.ID, "error_code", code, "error", err)
		failed, ferr := s.endIntent(ctx, intent, domain.IntentFailed, "", string(code))
		if ferr != nil {
			return Topup{}, ferr
		}
		if errors.Is(err, adapters.ErrInvalidRequest) {
			return topupOf(failed), fmt.Errorf("%w: %w", ErrValidation, err)
		}
		return topupOf(failed), nil
	}

	// tx 1: record the request ID and commit, then look for a callback
	// that arrived first. A callback handler parks its callback before
	// looking up the intent, so whichever of the two runs second sees
	// the other.
	if err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).MarkPaymentIntentPending(ctx, store.MarkPaymentIntentPendingParams{
			ID: intent.ID, RailRequestID: pgtype.Text{String: resp.CheckoutRequestID, Valid: true},
		})
		return err
	}); err != nil {
		return Topup{}, fmt.Errorf("app: recording the push: %w", err)
	}
	if err := s.applyParkedCallback(ctx, intent.RailAccountID, resp.CheckoutRequestID); err != nil {
		s.log.ErrorContext(ctx, "payments: applying a parked STK callback; the sweep retries it", "intent_id", intent.ID, "error", err)
	}
	return s.loadTopup(ctx, tenant, intent.ID)
}

func (s *Service) stkCallbackURL(railAccountID uuid.UUID) string {
	return s.cbBase + "/v1/payments/callbacks/daraja/" + railAccountID.String() + "/" + domain.CallbackToken(s.cbSecret, railAccountID) + "/stk"
}

func (s *Service) loadTopup(ctx context.Context, tenant, id uuid.UUID) (Topup, error) {
	var out Topup
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		r, err := store.New(tx).GetPaymentIntent(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no top-up %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("app: loading the top-up: %w", err)
		}
		out = topupOf(r)
		return nil
	})
	return out, err
}

// GetTopup is one of the tenant's top-ups (billing:read).
func (s *Service) GetTopup(ctx context.Context, user, tenant, topupID uuid.UUID) (Topup, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return Topup{}, err
	}
	return s.loadTopup(ctx, tenant, topupID)
}

// endIntent moves a non-final intent to a final failed status (failed,
// cancelled or expired) with its error code, and writes
// payment_intent.failed. An intent already final is left alone.
func (s *Service) endIntent(ctx context.Context, intent store.PaymentsPaymentIntent, status domain.IntentStatus, resultCode, errorCode string) (store.PaymentsPaymentIntent, error) {
	var out store.PaymentsPaymentIntent
	err := s.pool.WithTenantTx(ctx, intent.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetPaymentIntentForUpdate(ctx, intent.ID)
		if err != nil {
			return fmt.Errorf("app: locking the top-up: %w", err)
		}
		if domain.IntentStatus(cur.Status).Final() {
			out = cur
			return nil
		}
		out, err = q.CompletePaymentIntent(ctx, store.CompletePaymentIntentParams{
			ID: cur.ID, Status: store.PaymentsIntentStatus(status), ResultCode: text(resultCode), ErrorCode: text(errorCode),
			CompletedAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("app: ending the top-up: %w", err)
		}
		return s.writeIntentEvent(ctx, tx, out, 0, false)
	})
	return out, err
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// HandleSTKCallback receives Daraja's STK result for a rail account
// (task 4.4). The source and path token are checked before anything is
// read or written (ErrBadCallback). The callback is recorded once per
// checkout; a repeat of one already applied changes nothing, and one
// whose push hasn't been recorded yet waits until it is.
func (s *Service) HandleSTKCallback(ctx context.Context, railAccountID uuid.UUID, token string, clientIP netip.Addr, body []byte) error {
	if !domain.SourceAllowed(clientIP, s.cbCIDRs) || len(s.cbSecret) == 0 || !domain.ValidCallbackToken(s.cbSecret, railAccountID, token) {
		return ErrBadCallback
	}
	var cb store.PaymentsStkCallback
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		acct, err := q.GetRailAccount(ctx, railAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadCallback
		}
		if err != nil {
			return fmt.Errorf("app: loading the rail account: %w", err)
		}
		loc, err := s.railLocation(acct.RailCode)
		if err != nil {
			return err
		}
		result, err := adapters.ParseSTKCallback(body, loc)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrValidation, err)
		}
		id, err := ids.New()
		if err != nil {
			return err
		}
		cb, err = q.InsertSTKCallback(ctx, store.InsertSTKCallbackParams{
			ID: id, RailAccountID: railAccountID, CheckoutRequestID: result.CheckoutRequestID,
			MerchantRequestID: text(result.MerchantRequestID), ResultCode: strconv.Itoa(result.ResultCode), Raw: body,
		})
		if errors.Is(err, pgx.ErrNoRows) { // a repeat: the first copy is the record
			cb, err = q.GetSTKCallback(ctx, store.GetSTKCallbackParams{RailAccountID: railAccountID, CheckoutRequestID: result.CheckoutRequestID})
		}
		if err != nil {
			return fmt.Errorf("app: recording the callback: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if cb.AppliedAt.Valid {
		return nil
	}
	return s.applyCallback(ctx, cb)
}

// railLocation is the time zone of the country whose pack lists rail:
// Daraja's timestamps are local time.
func (s *Service) railLocation(rail string) (*time.Location, error) {
	p, err := s.railPack(rail)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return nil, fmt.Errorf("app: %s's time zone %q: %w", p.CountryCode, p.Timezone, err)
	}
	return loc, nil
}

// applyParkedCallback applies a recorded, unapplied callback for a
// checkout, if one arrived before its push was recorded.
func (s *Service) applyParkedCallback(ctx context.Context, railAccountID uuid.UUID, checkout string) error {
	var (
		cb    store.PaymentsStkCallback
		found bool
	)
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cb, err = store.New(tx).GetSTKCallback(ctx, store.GetSTKCallbackParams{RailAccountID: railAccountID, CheckoutRequestID: checkout})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found || cb.AppliedAt.Valid {
		return err
	}
	return s.applyCallback(ctx, cb)
}

// applyCallback applies a recorded callback to its intent: the outcome
// in the tenant's transaction, then the callback marked applied, then
// the ledger posting. Each step is safe to repeat, so a failure part
// way is finished by the sweep.
func (s *Service) applyCallback(ctx context.Context, cb store.PaymentsStkCallback) error {
	var (
		intent store.PaymentsPaymentIntent
		rail   string
		found  bool
	)
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		intent, err = q.GetPaymentIntentByRailRequest(ctx, store.GetPaymentIntentByRailRequestParams{
			RailAccountID: cb.RailAccountID, RailRequestID: text(cb.CheckoutRequestID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // parked until its push is recorded
		}
		if err != nil {
			return fmt.Errorf("app: finding the callback's top-up: %w", err)
		}
		found = true
		acct, err := q.GetRailAccount(ctx, cb.RailAccountID)
		rail = acct.RailCode
		return err
	})
	if err != nil || !found {
		return err
	}
	loc, err := s.railLocation(rail)
	if err != nil {
		return err
	}
	result, err := adapters.ParseSTKCallback(cb.Raw, loc)
	if err != nil {
		return fmt.Errorf("app: reading a recorded callback: %w", err)
	}

	notice, err := s.applyOutcome(ctx, intent, result, cb.Raw)
	if err != nil {
		return err
	}
	if err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).MarkSTKCallbackApplied(ctx, store.MarkSTKCallbackAppliedParams{
			ID: cb.ID, PaymentIntentID: pgtype.UUID{Bytes: intent.ID, Valid: true},
			AppliedAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		})
		return err
	}); err != nil {
		return fmt.Errorf("app: marking the callback applied: %w", err)
	}
	if notice != nil {
		s.postNotice(ctx, *notice, rail)
	}
	return nil
}

// applyOutcome decides a final STK result against the intent (with
// domain.ApplySTKResult) and writes it in the tenant's transaction: the
// intent's status, and for a success the collection notice the money
// arrived on, any exception (the callback's amount differs; it was
// reported failed first), and the event. It returns the notice to post,
// if this call created one.
func (s *Service) applyOutcome(ctx context.Context, intent store.PaymentsPaymentIntent, result adapters.STKResult, raw []byte) (*store.PaymentsCollectionNotice, error) {
	var notice *store.PaymentsCollectionNotice
	err := s.pool.WithTenantTx(ctx, intent.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetPaymentIntentForUpdate(ctx, intent.ID)
		if err != nil {
			return fmt.Errorf("app: locking the top-up: %w", err)
		}
		d := domain.ApplySTKResult(
			domain.IntentState{Status: domain.IntentStatus(cur.Status), AmountMinor: cur.AmountMinor, Receipt: cur.RailReceipt.String},
			domain.STKOutcome{ResultCode: result.ResultCode, ErrorCode: string(result.ErrorCode()), Receipt: result.Receipt, AmountMinor: result.AmountMinor},
		)
		if d.Duplicate {
			s.log.ErrorContext(ctx, "payments: a second receipt for a paid top-up; not applied", "intent_id", cur.ID)
		}
		if !d.Apply {
			return nil
		}
		now := s.clk.Now()
		if d.Status != domain.IntentSucceeded {
			ended, err := q.CompletePaymentIntent(ctx, store.CompletePaymentIntentParams{
				ID: cur.ID, Status: store.PaymentsIntentStatus(d.Status), ResultCode: text(strconv.Itoa(result.ResultCode)),
				ErrorCode: text(string(result.ErrorCode())), CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("app: ending the top-up: %w", err)
			}
			return s.writeIntentEvent(ctx, tx, ended, 0, false)
		}

		paid, err := q.CompletePaymentIntent(ctx, store.CompletePaymentIntentParams{
			ID: cur.ID, Status: store.PaymentsIntentStatusSucceeded, RailReceipt: text(result.Receipt),
			ResultCode: text("0"), CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("app: completing the top-up: %w", err)
		}
		id, err := ids.New()
		if err != nil {
			return err
		}
		n, err := q.InsertCollectionNotice(ctx, store.InsertCollectionNoticeParams{
			ID: id, RailAccountID: cur.RailAccountID, RailReceipt: result.Receipt, TransactionType: text("CustomerPayBillOnline"),
			AmountMinor: result.AmountMinor, Currency: cur.Currency, BillRef: text(cur.AccountReference),
			PayerPhoneE164: text(result.PhoneE164), PaidAt: result.PaidAt,
			PaymentIntentID: pgtype.UUID{Bytes: cur.ID, Valid: true}, MatchStatus: "matched",
			MatchedTenantID: pgtype.UUID{Bytes: cur.TenantID, Valid: true}, Raw: raw,
		})
		switch {
		case err == nil:
			notice = &n
		case errors.Is(err, pgx.ErrNoRows):
			// The receipt is already on a notice (a C2B confirmation of
			// the same payment): that one credits it.
		default:
			return fmt.Errorf("app: recording the payment: %w", err)
		}
		if d.AmountMismatch {
			if err := s.openException(ctx, q, cur, "amount_mismatch"); err != nil {
				return err
			}
		}
		if d.Late {
			if err := s.openException(ctx, q, cur, "status_mismatch"); err != nil {
				return err
			}
		}
		return s.writeIntentEvent(ctx, tx, paid, result.AmountMinor, d.Late)
	})
	return notice, err
}

// openException flags a client's top-up for staff, in its tenant's
// transaction: it carries the tenant and refers to the intent.
func (s *Service) openException(ctx context.Context, q *store.Queries, intent store.PaymentsPaymentIntent, kind string) error {
	id, err := ids.New()
	if err != nil {
		return err
	}
	_, err = q.CreateReconException(ctx, store.CreateReconExceptionParams{
		ID: id, TenantID: pgtype.UUID{Bytes: intent.TenantID, Valid: true}, RailAccountID: intent.RailAccountID,
		Kind: kind, ReferenceType: text("payment_intent"), ReferenceID: pgtype.UUID{Bytes: intent.ID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("app: opening a %s exception: %w", kind, err)
	}
	s.log.WarnContext(ctx, "payments: reconciliation exception opened", "kind", kind, "intent_id", intent.ID)
	return nil
}

// writeIntentEvent writes payment_intent.succeeded (with paidMinor and
// whether it was late) or payment_intent.failed for intent's status.
func (s *Service) writeIntentEvent(ctx context.Context, tx pgx.Tx, intent store.PaymentsPaymentIntent, paidMinor int64, late bool) error {
	data := map[string]any{
		"intent_id": intent.ID, "tenant_id": intent.TenantID, "purpose": intent.Purpose, "method": intent.Method,
		"status": intent.Status, "amount_minor": intent.AmountMinor, "currency": intent.Currency,
	}
	if intent.RailRequestID.Valid {
		data["rail_request_id"] = intent.RailRequestID.String
	}
	eventType := "payment_intent.failed"
	if intent.Status == store.PaymentsIntentStatusSucceeded {
		eventType = "payment_intent.succeeded"
		data["paid_minor"], data["receipt"], data["late"] = paidMinor, intent.RailReceipt.String, late
	} else {
		data["error_code"] = intent.ErrorCode.String
		if intent.ResultCode.Valid {
			data["result_code"] = intent.ResultCode.String
		}
	}
	eventID, err := ids.New()
	if err != nil {
		return err
	}
	env, err := platformevents.New(eventID, eventType, 1, intent.TenantID, s.clk.Now(), "", data)
	if err != nil {
		return fmt.Errorf("app: building %s: %w", eventType, err)
	}
	return outbox.Write(ctx, tx, "payment_intent", intent.ID, env)
}

// postNotice credits a matched notice to its tenant's main wallet:
// from provider clearing, or out of suspense for one staff matched
// (task 4.5), keyed by the notice; and records the transaction. A failure is logged and left for the sweep; the
// ledger's key idempotency makes a repeat return the same transaction.
func (s *Service) postNotice(ctx context.Context, n store.PaymentsCollectionNotice, rail string) bool {
	if s.ledger == nil || !n.MatchedTenantID.Valid {
		return false
	}
	kind := adapters.TopupDirect
	if n.SuspenseTransactionID.Valid { // matched by staff: out of suspense
		kind = adapters.TopupFromSuspense
	}
	txn, err := s.ledger.PostTopup(ctx, adapters.TopupPosting{
		Kind: kind, TenantID: uuid.UUID(n.MatchedTenantID.Bytes), NoticeID: n.ID, Rail: rail, Currency: n.Currency,
		AmountMinor: n.AmountMinor, Description: "M-Pesa top-up " + n.RailReceipt,
	})
	if err != nil {
		s.log.WarnContext(ctx, "payments: posting a top-up failed; the sweep retries it", "notice_id", n.ID, "error", err)
		return false
	}
	if err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).SetNoticeLedgerTransaction(ctx, store.SetNoticeLedgerTransactionParams{
			ID: n.ID, LedgerTransactionID: pgtype.UUID{Bytes: txn, Valid: true},
		})
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "payments: recording a posting failed; the sweep retries it", "notice_id", n.ID, "error", err)
		return false
	}
	return true
}

// SweepSummary counts what one sweep did.
type SweepSummary struct {
	Posted, Applied, Expired, Queried int
}

// Sweep finishes what callbacks left (task 4.4): it posts payments in
// suspense not yet in the ledger's suspense account (task 4.5) and
// matched notices not yet credited, applies callbacks whose push is now
// recorded, and settles top-ups past their expiry. A created top-up was
// never sent and expires. A pending one is asked about: a failure is
// applied; a payment the rail reports without a confirmation is flagged
// once and not credited (OQ-22); no answer past the give-up time
// expires it. Every replica may sweep: each step is safe to repeat.
func (s *Service) Sweep(ctx context.Context) (SweepSummary, error) {
	var (
		sum      SweepSummary
		notices  []store.PaymentsCollectionNotice
		unposted []store.PaymentsCollectionNotice // in suspense, not yet in the ledger's
		parked   []store.PaymentsStkCallback
		expiring []store.PaymentsPaymentIntent
		rails    = map[uuid.UUID]string{}
	)
	now := s.clk.Now()
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if notices, err = q.ListUnpostedNotices(ctx, sweepBatch); err != nil {
			return err
		}
		if unposted, err = q.ListUnpostedSuspense(ctx, sweepBatch); err != nil {
			return err
		}
		if parked, err = q.ListUnappliedSTKCallbacks(ctx, sweepBatch); err != nil {
			return err
		}
		if expiring, err = q.ListPaymentIntentsToExpire(ctx, store.ListPaymentIntentsToExpireParams{ExpiresAt: now, Limit: sweepBatch}); err != nil {
			return err
		}
		for _, n := range append(append([]store.PaymentsCollectionNotice{}, notices...), unposted...) {
			if _, ok := rails[n.RailAccountID]; !ok {
				acct, err := q.GetRailAccount(ctx, n.RailAccountID)
				if err != nil {
					return err
				}
				rails[n.RailAccountID] = acct.RailCode
			}
		}
		return nil
	})
	if err != nil {
		return sum, fmt.Errorf("app: listing the sweep's work: %w", err)
	}
	for _, n := range unposted {
		if s.postSuspense(ctx, n, rails[n.RailAccountID]) {
			sum.Posted++
		}
	}
	for _, n := range notices {
		if s.postNotice(ctx, n, rails[n.RailAccountID]) {
			sum.Posted++
		}
	}
	for _, cb := range parked {
		if err := s.applyCallback(ctx, cb); err != nil {
			s.log.WarnContext(ctx, "payments: applying a callback failed", "callback_id", cb.ID, "error", err)
			continue
		}
		sum.Applied++
	}
	for _, in := range expiring {
		expired, queried, err := s.settleExpired(ctx, in, now)
		if err != nil {
			s.log.WarnContext(ctx, "payments: settling an expired top-up failed", "intent_id", in.ID, "error", err)
		}
		if expired {
			sum.Expired++
		}
		if queried {
			sum.Queried++
		}
	}
	return sum, nil
}

// settleExpired settles one top-up past its expiry.
func (s *Service) settleExpired(ctx context.Context, in store.PaymentsPaymentIntent, now time.Time) (expired, queried bool, err error) {
	if in.Status == store.PaymentsIntentStatusCreated {
		_, err := s.endIntent(ctx, in, domain.IntentExpired, "", string(problem.PAY6507))
		return err == nil, false, err
	}
	giveUp := now.After(in.ExpiresAt.Add(s.giveUp))
	expire := func() (bool, bool, error) {
		if !giveUp {
			return false, true, nil
		}
		_, err := s.endIntent(ctx, in, domain.IntentExpired, "", string(problem.PAY6504))
		return err == nil, true, err
	}
	rail, err := s.railOfAccount(ctx, in.RailAccountID)
	if err != nil {
		return false, false, err
	}
	client, ok := s.rails[rail]
	if !ok {
		return false, false, fmt.Errorf("app: no client for rail %s", rail)
	}
	res, err := client.STKQuery(ctx, in.RailAccountID, in.RailRequestID.String)
	if err != nil {
		s.log.WarnContext(ctx, "payments: STK query failed", "intent_id", in.ID, "error", err)
		return expire()
	}
	if res.Pending {
		return expire()
	}
	if res.ResultCode != 0 {
		_, err := s.applyOutcome(ctx, in, res, nil)
		return false, true, err
	}
	// The rail says paid, but there's no confirmation: no receipt, no
	// amount. Nothing is credited; staff are told once (OQ-22), and a
	// late callback, a C2B confirmation or the statement settles it.
	err = s.pool.WithTenantTx(ctx, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetPaymentIntentForUpdate(ctx, in.ID)
		if err != nil {
			return err
		}
		if cur.Status != store.PaymentsIntentStatusPending || cur.ResultCode.Valid {
			return nil
		}
		if err := q.SetPaymentIntentResultCode(ctx, store.SetPaymentIntentResultCodeParams{ID: cur.ID, ResultCode: text("0")}); err != nil {
			return err
		}
		return s.openException(ctx, q, cur, "status_mismatch")
	})
	return false, true, err
}

func (s *Service) railOfAccount(ctx context.Context, id uuid.UUID) (string, error) {
	var rail string
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		acct, err := store.New(tx).GetRailAccount(ctx, id)
		rail = acct.RailCode
		return err
	})
	return rail, err
}
