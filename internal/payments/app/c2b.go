package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// operationsRole is staff's read role (migration 20261111090000).
const operationsRole = "payments_operations"

var (
	// ErrStepUpRequired: the session's step-up isn't fresh (AUTH-4012).
	ErrStepUpRequired = errors.New("app: step-up required")
	// ErrNoticeDecided: the payment isn't in suspense any more (VAL-4014).
	ErrNoticeDecided = errors.New("app: the payment was already matched")
	// ErrSuspenseNotPosted: the payment isn't in the ledger's suspense
	// account yet, so it can't be moved out (PAY-6507, retry shortly).
	ErrSuspenseNotPosted = errors.New("app: the payment isn't in the ledger's suspense account yet")
)

// c2bRefused is Daraja's "other error" result for a validation call.
const c2bRefused = "C2B00016"

// Notice is a payment to VASPRO's paybill as staff see it.
type Notice struct {
	ID          uuid.UUID
	Receipt     string
	AmountMinor int64
	Currency    string
	BillRef     string
	PayerName   string
	PayerPhone  string
	PaidAt      time.Time
	Status      string
	TenantID    *uuid.UUID
}

func noticeOf(r store.PaymentsCollectionNotice) Notice {
	n := Notice{
		ID: r.ID, Receipt: r.RailReceipt, AmountMinor: r.AmountMinor, Currency: r.Currency, BillRef: r.BillRef.String,
		PayerName: r.PayerName.String, PayerPhone: r.PayerPhoneE164.String, PaidAt: r.PaidAt, Status: r.MatchStatus,
	}
	if r.MatchedTenantID.Valid {
		id := uuid.UUID(r.MatchedTenantID.Bytes)
		n.TenantID = &id
	}
	return n
}

// paybillCallback checks a callback's source and path token (OQ-22) and
// loads the VASPRO paybill it's for, and the pack its rail is in.
func (s *Service) paybillCallback(ctx context.Context, railAccountID uuid.UUID, token string, clientIP netip.Addr) (store.PaymentsRailAccount, *packs.Pack, error) {
	if !domain.SourceAllowed(clientIP, s.cbCIDRs) || len(s.cbSecret) == 0 || !domain.ValidCallbackToken(s.cbSecret, railAccountID, token) {
		return store.PaymentsRailAccount{}, nil, ErrBadCallback
	}
	var acct store.PaymentsRailAccount
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = store.New(tx).GetRailAccount(ctx, railAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadCallback
		}
		return err
	})
	if err != nil {
		return acct, nil, err
	}
	p, err := s.railPack(acct.RailCode)
	return acct, p, err
}

// railPack is the pack that lists rail.
func (s *Service) railPack(rail string) (*packs.Pack, error) {
	for _, p := range s.packs {
		for _, r := range p.Rails {
			if r.Code == rail {
				return p, nil
			}
		}
	}
	return nil, fmt.Errorf("app: no pack lists rail %s", rail)
}

func (s *Service) parseC2B(p *packs.Pack, body []byte) (adapters.C2BPayment, error) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return adapters.C2BPayment{}, fmt.Errorf("app: %s's time zone: %w", p.CountryCode, err)
	}
	pay, err := adapters.ParseC2BCallback(body, loc)
	if err != nil {
		return adapters.C2BPayment{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return pay, nil
}

// HandleC2BValidation answers Daraja's validation call (task 4.5). It
// accepts every payment that could be VASPRO's, unknown account numbers
// included, since they go to suspense (OQ-22); only a payment to another
// shortcode is refused. It writes nothing.
func (s *Service) HandleC2BValidation(ctx context.Context, railAccountID uuid.UUID, token string, clientIP netip.Addr, body []byte) (bool, string, error) {
	acct, p, err := s.paybillCallback(ctx, railAccountID, token, clientIP)
	if err != nil {
		return false, "", err
	}
	pay, err := s.parseC2B(p, body)
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(pay.ShortCode) != acct.Number {
		return false, c2bRefused, nil
	}
	return true, "", nil
}

// HandleC2BConfirmation records a payment to VASPRO's paybill (task 4.5).
// A valid, issued account number credits that client's main wallet;
// anything else goes to suspense (topup.unmatched) for staff. A repeat,
// or a receipt an STK callback already recorded, changes nothing.
func (s *Service) HandleC2BConfirmation(ctx context.Context, railAccountID uuid.UUID, token string, clientIP netip.Addr, body []byte) error {
	acct, p, err := s.paybillCallback(ctx, railAccountID, token, clientIP)
	if err != nil {
		return err
	}
	pay, err := s.parseC2B(p, body)
	if err != nil {
		return err
	}
	if strings.TrimSpace(pay.ShortCode) != acct.Number {
		return fmt.Errorf("%w: a confirmation for shortcode %s, not this paybill", ErrValidation, pay.ShortCode)
	}
	number, named := domain.MatchC2B(pay.BillRef, pay.ShortCode, acct.Number)

	var notice *store.PaymentsCollectionNotice
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		params := store.InsertCollectionNoticeParams{
			RailAccountID: acct.ID, RailReceipt: pay.Receipt, TransactionType: text(pay.TransactionType),
			AmountMinor: pay.AmountMinor, Currency: p.Currency.Code, BillRef: text(pay.BillRef),
			PayerPhoneE164: text(pay.PhoneE164), PayerName: text(pay.PayerName), PaidAt: pay.PaidAt,
			MatchStatus: "suspense", Raw: body,
		}
		if pay.OrgBalanceMinor != nil {
			params.AccountBalanceMinor = pgtype.Int8{Int64: *pay.OrgBalanceMinor, Valid: true}
		}
		if named {
			holder, err := q.GetTopupAccountByNumber(ctx, store.GetTopupAccountByNumberParams{RailAccountID: acct.ID, AccountNumber: number})
			switch {
			case err == nil:
				params.MatchStatus = "matched"
				params.MatchedTenantID = pgtype.UUID{Bytes: holder.TenantID, Valid: true}
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("app: looking up the account number: %w", err)
			}
		}
		id, err := ids.New()
		if err != nil {
			return err
		}
		params.ID = id
		n, err := q.InsertCollectionNotice(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // recorded already
		}
		if err != nil {
			return fmt.Errorf("app: recording the payment: %w", err)
		}
		notice = &n
		if n.MatchStatus == "suspense" {
			return s.writeNoticeEvent(ctx, tx, "topup.unmatched", n, uuid.Nil, map[string]any{
				"notice_id": n.ID, "rail_account_id": n.RailAccountID, "receipt": n.RailReceipt,
				"amount_minor": n.AmountMinor, "currency": n.Currency, "paid_at": n.PaidAt,
			})
		}
		return nil
	})
	if err != nil || notice == nil {
		return err
	}
	if notice.MatchStatus == "suspense" {
		s.postSuspense(ctx, *notice, acct.RailCode)
	} else {
		s.postNotice(ctx, *notice, acct.RailCode)
	}
	return nil
}

func (s *Service) writeNoticeEvent(ctx context.Context, tx pgx.Tx, eventType string, n store.PaymentsCollectionNotice, tenant uuid.UUID, data map[string]any) error {
	eventID, err := ids.New()
	if err != nil {
		return err
	}
	env, err := platformevents.New(eventID, eventType, 1, tenant, s.clk.Now(), "", data)
	if err != nil {
		return fmt.Errorf("app: building %s: %w", eventType, err)
	}
	return outbox.Write(ctx, tx, "collection_notice", n.ID, env)
}

// postSuspense puts an unmatched payment into the ledger's suspense
// account and records the posting. A failure is left for the sweep.
func (s *Service) postSuspense(ctx context.Context, n store.PaymentsCollectionNotice, rail string) bool {
	if s.ledger == nil {
		return false
	}
	txn, err := s.ledger.PostTopup(ctx, adapters.TopupPosting{
		Kind: adapters.TopupToSuspense, NoticeID: n.ID, Rail: rail, Currency: n.Currency,
		AmountMinor: n.AmountMinor, Description: "Unmatched M-Pesa payment " + n.RailReceipt,
	})
	if err != nil {
		s.log.WarnContext(ctx, "payments: posting to suspense failed; the sweep retries it", "notice_id", n.ID, "error", err)
		return false
	}
	if err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).SetNoticeSuspenseTransaction(ctx, store.SetNoticeSuspenseTransactionParams{
			ID: n.ID, SuspenseTransactionID: pgtype.UUID{Bytes: txn, Valid: true},
		})
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "payments: recording a suspense posting failed; the sweep retries it", "notice_id", n.ID, "error", err)
		return false
	}
	return true
}

// requireStaffID asks identity whether userID is staff, on every call.
func (s *Service) requireStaffID(ctx context.Context, userID uuid.UUID) error {
	if s.identity == nil {
		return errors.New("app: no identity adapter for staff checks")
	}
	staff, err := s.identity.IsStaff(ctx, userID)
	if err != nil {
		return fmt.Errorf("app: checking staff: %w", err)
	}
	if !staff {
		return ErrNotStaff
	}
	return nil
}

// ListSuspense is the staff queue of payments in suspense, oldest first
// (task 4.5), paged by an opaque cursor.
func (s *Service) ListSuspense(ctx context.Context, staff uuid.UUID, cursor string, limit int) ([]Notice, string, error) {
	if err := s.requireStaffID(ctx, staff); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		return nil, "", fmt.Errorf("%w: limit is at most 100", ErrValidation)
	}
	afterAt, afterID, err := decodeNoticeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var rows []store.PaymentsCollectionNotice
	err = s.pool.WithRoleTx(ctx, operationsRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListSuspense(ctx, store.ListSuspenseParams{AfterPaidAt: afterAt, AfterID: afterID, MaxRows: int32(limit + 1)}) //nolint:gosec // limit is 1..100
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("app: listing suspense: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeNoticeCursor(last.PaidAt, last.ID)
	}
	out := make([]Notice, len(rows))
	for i, r := range rows {
		out[i] = noticeOf(r)
	}
	return out, next, nil
}

func encodeNoticeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeNoticeCursor(c string) (time.Time, uuid.UUID, error) {
	if c == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad cursor", ErrValidation)
	}
	atText, idText, ok := strings.Cut(string(raw), "|")
	at, err1 := time.Parse(time.RFC3339Nano, atText)
	id, err2 := uuid.Parse(idText)
	if !ok || err1 != nil || err2 != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad cursor", ErrValidation)
	}
	return at, id, nil
}

// MatchNotice credits a payment in suspense to the client whose account
// number staff name (task 4.5): staff with a fresh step-up, a reason,
// an issued number. The notice is locked and moves out of suspense once,
// and only once the ledger has it in suspense (ErrSuspenseNotPosted
// before), so the money is never both in suspense and credited from
// provider clearing.
func (s *Service) MatchNotice(ctx context.Context, staff, session, noticeID uuid.UUID, accountNumber, reason string) (Notice, error) {
	if err := s.requireStaffID(ctx, staff); err != nil {
		return Notice{}, err
	}
	fresh, err := s.identity.IsStepUpFresh(ctx, staff, session)
	if err != nil {
		return Notice{}, fmt.Errorf("app: checking step-up: %w", err)
	}
	if !fresh {
		return Notice{}, ErrStepUpRequired
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Notice{}, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	number, err := domain.NormalizeAccountNumber(accountNumber)
	if err != nil {
		return Notice{}, fmt.Errorf("%w: %q isn't an account number", ErrNotFound, accountNumber)
	}

	var (
		matched store.PaymentsCollectionNotice
		rail    string
	)
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.GetCollectionNoticeForUpdate(ctx, noticeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no payment %s on VASPRO's paybill", ErrNotFound, noticeID)
		}
		if err != nil {
			return fmt.Errorf("app: locking the payment: %w", err)
		}
		if n.MatchStatus != "suspense" {
			return ErrNoticeDecided
		}
		if !n.SuspenseTransactionID.Valid {
			return ErrSuspenseNotPosted
		}
		holder, err := q.GetTopupAccountByNumber(ctx, store.GetTopupAccountByNumberParams{RailAccountID: n.RailAccountID, AccountNumber: number})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no client has account number %s", ErrNotFound, number)
		}
		if err != nil {
			return fmt.Errorf("app: looking up the account number: %w", err)
		}
		// The update only moves a payment still in suspense, a second
		// guard behind the lock.
		matched, err = q.MatchCollectionNotice(ctx, store.MatchCollectionNoticeParams{ID: n.ID, MatchedTenantID: pgtype.UUID{Bytes: holder.TenantID, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoticeDecided
		}
		if err != nil {
			return fmt.Errorf("app: matching the payment: %w", err)
		}
		acct, err := q.GetRailAccount(ctx, n.RailAccountID)
		if err != nil {
			return err
		}
		rail = acct.RailCode
		return s.writeNoticeEvent(ctx, tx, "collection_notice.matched", matched, holder.TenantID, map[string]any{
			"notice_id": matched.ID, "tenant_id": holder.TenantID, "rail_account_id": matched.RailAccountID,
			"receipt": matched.RailReceipt, "amount_minor": matched.AmountMinor, "currency": matched.Currency,
			"account_number": number, "matched_by": staff, "reason": reason,
		})
	})
	if err != nil {
		return Notice{}, err
	}
	s.log.InfoContext(ctx, "payments: payment matched out of suspense", "notice_id", matched.ID, "staff_id", staff)
	s.postNotice(ctx, matched, rail)
	return noticeOf(matched), nil
}

// RegisterC2B registers a VASPRO paybill's C2B confirmation and
// validation URLs with its rail (ops CLI, task 4.5), staff only.
func (s *Service) RegisterC2B(ctx context.Context, staffEmail string, railAccountID uuid.UUID) error {
	if _, err := s.requireStaff(ctx, staffEmail); err != nil {
		return err
	}
	var acct store.PaymentsRailAccount
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = store.New(tx).GetRailAccount(ctx, railAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no rail account %s", ErrNotFound, railAccountID)
		}
		return err
	})
	if err != nil {
		return err
	}
	client, ok := s.rails[acct.RailCode]
	if !ok {
		return fmt.Errorf("app: no client for rail %s", acct.RailCode)
	}
	base := s.cbBase + "/v1/payments/callbacks/daraja/" + acct.ID.String() + "/" + domain.CallbackToken(s.cbSecret, acct.ID) + "/c2b/"
	if err := client.RegisterC2BURLs(ctx, acct.ID, base+"confirmation", base+"validation"); err != nil {
		return fmt.Errorf("app: registering the C2B URLs: %w", err)
	}
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).SetRailAccountC2BRegistered(ctx, store.SetRailAccountC2BRegisteredParams{
			ID: acct.ID, C2bRegisteredAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		})
		return err
	})
}
