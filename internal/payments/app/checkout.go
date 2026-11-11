package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

var (
	// ErrCheckoutPaid: the link was already paid (VAL-4014).
	ErrCheckoutPaid = errors.New("app: the checkout link has been paid")
	// ErrTooManyAttempts: the link has had its payment attempts
	// (RATE-4290).
	ErrTooManyAttempts = errors.New("app: too many payment attempts on this link")
)

// Checkout link limits (task 4.8).
const (
	DefaultCheckoutTTL = 24 * time.Hour
	minCheckoutTTL     = 5 * time.Minute
	maxCheckoutTTL     = 7 * 24 * time.Hour
	maxCheckoutTries   = 5
)

// CheckoutLink is a link a client sends to whoever pays.
type CheckoutLink struct {
	LinkID      uuid.UUID
	Token       string
	URL         string
	AmountMinor int64
	Currency    string
	ExpiresAt   time.Time
}

// CheckoutView is what a link's page shows.
type CheckoutView struct {
	AmountMinor      int64
	Currency         string
	AmountText       string // e.g. "5000.00 KES", in the currency's own decimals
	Payee            string
	AccountReference string
	Status           string // open, paid or expired
	ExpiresAt        time.Time
}

// CreateCheckoutLink makes a signed link to top up tenant's main wallet
// by amountMinor (task 4.8), which anyone can pay with no sign-in until
// it expires or is paid. Links are stateless: nothing is stored until
// someone pays. billing:manage.
func (s *Service) CreateCheckoutLink(ctx context.Context, user, tenant uuid.UUID, amountMinor int64, ttl time.Duration) (CheckoutLink, error) {
	info, err := s.requireTenant(ctx, user, tenant, PermBillingManage)
	if err != nil {
		return CheckoutLink{}, err
	}
	if len(s.coSecret) == 0 {
		return CheckoutLink{}, errors.New("app: no checkout secret configured")
	}
	if ttl == 0 {
		ttl = DefaultCheckoutTTL
	}
	if ttl < minCheckoutTTL || ttl > maxCheckoutTTL {
		return CheckoutLink{}, fmt.Errorf("%w: a link lasts 5 minutes to 7 days", ErrValidation)
	}
	pack, ok := s.packs[info.CountryCode]
	if !ok {
		return CheckoutLink{}, fmt.Errorf("%w: no pack for %s", ErrTopupUnavailable, info.CountryCode)
	}
	if err := domain.ValidateTopupAmount(amountMinor, pack.Currency.Exponent); err != nil {
		return CheckoutLink{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	if _, _, err := s.checkoutPaybill(ctx, tenant, pack); err != nil {
		return CheckoutLink{}, err
	}
	id, err := ids.New()
	if err != nil {
		return CheckoutLink{}, err
	}
	c := domain.Checkout{LinkID: id, TenantID: tenant, AmountMinor: amountMinor, Currency: info.Currency, Expires: s.clk.Now().Add(ttl).Truncate(time.Second).UTC()}
	token := domain.SignCheckout(s.coSecret, c)
	return CheckoutLink{
		LinkID: id, Token: token, URL: s.coBase + "/pay/" + token,
		AmountMinor: amountMinor, Currency: c.Currency, ExpiresAt: c.Expires,
	}, nil
}

// checkoutPaybill is the active paybill the tenant tops up on, and the
// tenant's number on it (issued if needed).
func (s *Service) checkoutPaybill(ctx context.Context, tenant uuid.UUID, pack *packs.Pack) (store.PaymentsRailAccount, string, error) {
	rail, ok := pack.TopupRail()
	if !ok {
		return store.PaymentsRailAccount{}, "", fmt.Errorf("%w: %s has no top-up rail", ErrTopupUnavailable, pack.CountryCode)
	}
	var (
		paybill store.PaymentsRailAccount
		number  string
	)
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		paybill, err = q.GetActiveTopupRailAccount(ctx, rail.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no verified paybill on %s", ErrTopupUnavailable, rail.Code)
		}
		if err != nil {
			return err
		}
		number, err = s.ensureTopupAccount(ctx, q, tenant, paybill.ID)
		return err
	})
	return paybill, number, err
}

// parseCheckout reads a token: one VASPRO didn't sign is ErrNotFound.
// An expired link is returned with expired set.
func (s *Service) parseCheckout(token string) (domain.Checkout, *packs.Pack, bool, error) {
	c, err := domain.ParseCheckout(s.coSecret, token, s.clk.Now())
	expired := errors.Is(err, domain.ErrCheckoutExpired)
	if err != nil && !expired {
		return c, nil, false, fmt.Errorf("%w: not a checkout link", ErrNotFound)
	}
	for _, p := range s.packs {
		if p.Currency.Code == c.Currency {
			return c, p, expired, nil
		}
	}
	return c, nil, expired, fmt.Errorf("%w: no pack for %s", ErrTopupUnavailable, c.Currency)
}

func (s *Service) checkoutPaid(ctx context.Context, link uuid.UUID) (paid bool, attempts int64, err error) {
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		id := pgtype.UUID{Bytes: link, Valid: true}
		var err error
		if paid, err = q.CheckoutPaid(ctx, id); err != nil {
			return err
		}
		attempts, err = q.CountCheckoutAttempts(ctx, id)
		return err
	})
	return paid, attempts, err
}

// GetCheckout is what a link asks for, and whether it's open, paid or
// expired (task 4.8).
func (s *Service) GetCheckout(ctx context.Context, token string) (CheckoutView, error) {
	c, pack, expired, err := s.parseCheckout(token)
	if err != nil {
		return CheckoutView{}, err
	}
	paybill, number, err := s.checkoutPaybill(ctx, c.TenantID, pack)
	if err != nil {
		return CheckoutView{}, err
	}
	paid, _, err := s.checkoutPaid(ctx, c.LinkID)
	if err != nil {
		return CheckoutView{}, err
	}
	status := "open"
	switch {
	case paid:
		status = "paid"
	case expired:
		status = "expired"
	}
	return CheckoutView{
		AmountMinor: c.AmountMinor, Currency: c.Currency, AmountText: money.New(c.AmountMinor, pack.Currency).String(), Payee: paybill.DisplayName,
		AccountReference: number, Status: status, ExpiresAt: c.Expires,
	}, nil
}

// PayCheckout prompts the payer's phone to pay a link (task 4.8). A
// repeated key returns the same payment without prompting again; a paid
// link is ErrCheckoutPaid; an expired or foreign one ErrNotFound; a
// link's sixth attempt ErrTooManyAttempts.
func (s *Service) PayCheckout(ctx context.Context, token, idempotencyKey, phone string) (Topup, error) {
	c, pack, expired, err := s.parseCheckout(token)
	if err != nil {
		return Topup{}, err
	}
	if expired {
		return Topup{}, fmt.Errorf("%w: the link has expired", ErrNotFound)
	}
	if idempotencyKey == "" {
		return Topup{}, fmt.Errorf("%w: an idempotency key is required", ErrValidation)
	}
	key := "checkout:" + c.LinkID.String() + ":" + idempotencyKey
	replay := false
	if err := s.pool.WithTenantTx(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).GetPaymentIntentByIdempotencyKey(ctx, store.GetPaymentIntentByIdempotencyKeyParams{TenantID: c.TenantID, IdempotencyKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		replay = err == nil
		return err
	}); err != nil {
		return Topup{}, err
	}
	if !replay {
		paid, attempts, err := s.checkoutPaid(ctx, c.LinkID)
		switch {
		case err != nil:
			return Topup{}, err
		case paid:
			return Topup{}, ErrCheckoutPaid
		case attempts >= maxCheckoutTries:
			return Topup{}, ErrTooManyAttempts
		}
	}
	return s.startTopup(ctx, c.TenantID, pack.CountryCode, c.Currency, key, c.AmountMinor, phone, &c.LinkID)
}

// GetCheckoutPayment is one of a link's payments.
func (s *Service) GetCheckoutPayment(ctx context.Context, token string, paymentID uuid.UUID) (Topup, error) {
	c, _, _, err := s.parseCheckout(token)
	if err != nil {
		return Topup{}, err
	}
	var out Topup
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		r, err := store.New(tx).GetCheckoutPayment(ctx, store.GetCheckoutPaymentParams{ID: paymentID, PaymentLinkID: pgtype.UUID{Bytes: c.LinkID, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such payment on this link", ErrNotFound)
		}
		if err != nil {
			return err
		}
		out = topupOf(r)
		return nil
	})
	return out, err
}
