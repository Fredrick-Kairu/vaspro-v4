package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// ErrUnknownService means a country pack names a service that isn't in
// the catalogue.
var ErrUnknownService = errors.New("app: unknown service")

// AccountService is one catalogue service as an account sees it.
type AccountService struct {
	Code     string
	Name     string
	MinTier  domain.Tier // in the account's country
	Selected bool
	Status   string // for a selected service: selected, enabled or suspended
	Locked   bool   // selected, but the account's tier is below MinTier
}

// ServiceCatalogue is the services available to an account.
type ServiceCatalogue struct {
	Tier         domain.Tier
	RequiredTier domain.Tier // the lowest tier unlocking everything selected
	Services     []AccountService
}

// SyncServiceRules copies each country pack's service rules into
// service_country_rules (task 2.4, OQ-06: the pack is the source, the
// table serves queries): rules are upserted, and a country's rules for
// services its pack no longer lists are deleted, so they fall back to
// the catalogue default. A code outside the catalogue is
// ErrUnknownService and nothing is written. Run calls it at startup.
func (s *Service) SyncServiceRules(ctx context.Context, loaded map[string]*packs.Pack) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		catalogue, err := q.ListServices(ctx)
		if err != nil {
			return fmt.Errorf("app: listing services: %w", err)
		}
		known := make(map[string]bool, len(catalogue))
		for _, svc := range catalogue {
			known[svc.Code] = true
		}
		for _, country := range slices.Sorted(mapKeys(loaded)) {
			pack := loaded[country]
			keep := slices.Sorted(mapKeys(pack.Services))
			for _, code := range keep {
				if !known[code] {
					return fmt.Errorf("%w: %q in the %s pack", ErrUnknownService, code, country)
				}
				rule := pack.Services[code]
				if err := q.UpsertServiceCountryRule(ctx, store.UpsertServiceCountryRuleParams{
					ServiceCode: code, CountryCode: country, MinTier: store.AccountsTier(rule.MinTier), Enabled: rule.Enabled,
				}); err != nil {
					return fmt.Errorf("app: saving the %s rule for %s: %w", code, country, err)
				}
			}
			if err := q.DeleteServiceCountryRulesExcept(ctx, store.DeleteServiceCountryRulesExceptParams{CountryCode: country, Keep: keep}); err != nil {
				return fmt.Errorf("app: removing old rules for %s: %w", country, err)
			}
		}
		return nil
	})
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// ListServices is tenantID's catalogue (account:read): every service
// available in its country, and any it selected, with the tier each
// needs there; a selected service above the account's tier is locked
// and reports that tier (system design §2).
func (s *Service) ListServices(ctx context.Context, userID, tenantID uuid.UUID) (*ServiceCatalogue, error) {
	tenant, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountRead)
	if err != nil {
		return nil, err
	}
	var cat *ServiceCatalogue
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rules, err := q.ListServicesForCountry(ctx, tenant.CountryCode)
		if err != nil {
			return fmt.Errorf("app: listing services: %w", err)
		}
		chosen, err := q.ListTenantServices(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing the account's services: %w", err)
		}
		status := make(map[string]string, len(chosen))
		for _, c := range chosen {
			status[c.ServiceCode] = c.Status
		}
		tier := domain.Tier(tenant.Tier)
		cat = &ServiceCatalogue{Tier: tier}
		var needed []domain.Tier
		for _, r := range rules {
			st, selected := status[r.Code]
			if !r.Enabled && !selected {
				continue
			}
			view := accountService(r, tier, selected, st)
			if selected {
				needed = append(needed, view.MinTier)
			}
			cat.Services = append(cat.Services, view)
		}
		cat.RequiredTier = domain.RequiredTier(needed)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cat, nil
}

func accountService(r store.ListServicesForCountryRow, tier domain.Tier, selected bool, status string) AccountService {
	minTier := domain.Tier(r.MinTier)
	return AccountService{
		Code: r.Code, Name: r.Name, MinTier: minTier,
		Selected: selected, Status: status,
		Locked: selected && !domain.Unlocked(tier, minTier),
	}
}

// SelectService adds code to tenantID's services (account:manage):
// enabled at once if the account's tier reaches the service's minimum
// in its country, otherwise selected and locked until verification
// reaches it. It writes service.selected, and service.enabled when
// enabled at once. A service that doesn't exist or is off in the
// country is ErrNotFound; one already selected is returned unchanged.
func (s *Service) SelectService(ctx context.Context, userID, tenantID uuid.UUID, code string) (*AccountService, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, err
	}
	var view AccountService
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		tenant, err := q.GetTenantForUpdate(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		rule, ok, err := serviceRule(ctx, q, tenant.CountryCode, code)
		if err != nil {
			return err
		}
		if !ok || !rule.Enabled {
			return ErrNotFound
		}
		tier := domain.Tier(tenant.Tier)
		existing, err := q.GetTenantService(ctx, store.GetTenantServiceParams{TenantID: tenantID, ServiceCode: code})
		if err == nil {
			view = accountService(rule, tier, true, existing.Status)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: loading the account's service: %w", err)
		}

		status := domain.ServiceStatusFor(tier, domain.Tier(rule.MinTier), domain.ServiceSelected)
		if _, err := q.CreateTenantService(ctx, store.CreateTenantServiceParams{TenantID: tenantID, ServiceCode: code, Status: domain.ServiceSelected}); err != nil {
			return fmt.Errorf("app: selecting the service: %w", err)
		}
		if err := s.writeTenantEvent(ctx, tx, tenantID, "service.selected", map[string]any{
			"tenant_id": tenantID, "service_code": code, "min_tier": rule.MinTier, "status": status, "selected_by": userID,
		}); err != nil {
			return err
		}
		if status == domain.ServiceEnabled {
			if err := s.setServiceStatus(ctx, q, tx, tenantID, code, rule.MinTier, status); err != nil {
				return err
			}
		}
		view = accountService(rule, tier, true, status)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &view, nil
}

// DeselectService removes code from tenantID's services
// (account:manage), writing service.deselected. A service that isn't
// selected is ErrNotFound.
func (s *Service) DeselectService(ctx context.Context, userID, tenantID uuid.UUID, code string) error {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetTenantForUpdate(ctx, tenantID); err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		n, err := q.DeleteTenantService(ctx, store.DeleteTenantServiceParams{TenantID: tenantID, ServiceCode: code})
		if err != nil {
			return fmt.Errorf("app: deselecting the service: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return s.writeTenantEvent(ctx, tx, tenantID, "service.deselected", map[string]any{
			"tenant_id": tenantID, "service_code": code, "deselected_by": userID,
		})
	})
}

// reevaluateServices brings tenantID's selected services in line with
// its new tier, in ChangeTier's transaction (OQ-06): newly unlocked ones
// are enabled (service.enabled); ones now above the tier go back to
// selected, locked (service.locked); suspended ones are left alone.
func (s *Service) reevaluateServices(ctx context.Context, q *store.Queries, tx pgx.Tx, tenantID uuid.UUID, country string, tier domain.Tier) error {
	rules, err := q.ListServicesForCountry(ctx, country)
	if err != nil {
		return fmt.Errorf("app: listing services: %w", err)
	}
	minTier := make(map[string]store.AccountsTier, len(rules))
	for _, r := range rules {
		minTier[r.Code] = r.MinTier
	}
	chosen, err := q.ListTenantServices(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("app: listing the account's services: %w", err)
	}
	for _, c := range chosen {
		min, ok := minTier[c.ServiceCode]
		if !ok {
			continue
		}
		next := domain.ServiceStatusFor(tier, domain.Tier(min), c.Status)
		if next == c.Status {
			continue
		}
		if err := s.setServiceStatus(ctx, q, tx, tenantID, c.ServiceCode, min, next); err != nil {
			return err
		}
	}
	return nil
}

// setServiceStatus moves a selected service to enabled or back to
// selected (locked), with its event.
func (s *Service) setServiceStatus(ctx context.Context, q *store.Queries, tx pgx.Tx, tenantID uuid.UUID, code string, minTier store.AccountsTier, status string) error {
	if err := q.SetTenantServiceStatus(ctx, store.SetTenantServiceStatusParams{Status: status, TenantID: tenantID, ServiceCode: code}); err != nil {
		return fmt.Errorf("app: updating the service: %w", err)
	}
	eventType := "service.enabled"
	if status == domain.ServiceSelected {
		eventType = "service.locked"
	}
	return s.writeTenantEvent(ctx, tx, tenantID, eventType, map[string]any{
		"tenant_id": tenantID, "service_code": code, "min_tier": minTier,
	})
}

// serviceRule is code's effective rule in country.
func serviceRule(ctx context.Context, q *store.Queries, country, code string) (store.ListServicesForCountryRow, bool, error) {
	rules, err := q.ListServicesForCountry(ctx, country)
	if err != nil {
		return store.ListServicesForCountryRow{}, false, fmt.Errorf("app: listing services: %w", err)
	}
	for _, r := range rules {
		if r.Code == code {
			return r, true, nil
		}
	}
	return store.ListServicesForCountryRow{}, false, nil
}
