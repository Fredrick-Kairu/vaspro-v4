// Package api serves accounts' HTTP API (docs/api/accounts.yaml).
// Every request arrives through the gateway, which has already checked
// the dashboard session and forwarded the user (task 2.2a).
package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

// Handlers implements StrictServerInterface over app.Service.
type Handlers struct {
	app *app.Service
}

// NewHandlers builds Handlers.
func NewHandlers(a *app.Service) *Handlers { return &Handlers{app: a} }

func (h *Handlers) CreateTenant(ctx context.Context, request CreateTenantRequestObject) (CreateTenantResponseObject, error) {
	body := request.Body
	in := domain.NewTenantInput{DisplayName: body.DisplayName, CountryCode: body.CountryCode}
	if body.LegalName != nil {
		in.LegalName = *body.LegalName
	}
	if body.BillingEmail != nil {
		in.BillingEmail = *body.BillingEmail
	}
	tenant, err := h.app.CreateTenant(ctx, callerFromContext(ctx), in)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTenant201JSONResponse(tenantResponse(tenant)), nil
}

func (h *Handlers) GetTenant(ctx context.Context, request GetTenantRequestObject) (GetTenantResponseObject, error) {
	id, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	tenant, err := h.app.GetTenant(ctx, callerFromContext(ctx), id)
	if err != nil {
		return nil, toProblem(err)
	}
	return GetTenant200JSONResponse(tenantResponse(tenant)), nil
}

func (h *Handlers) CreateSubAccount(ctx context.Context, request CreateSubAccountRequestObject) (CreateSubAccountResponseObject, error) {
	parentID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	body := request.Body
	in := app.SubAccountInput{DisplayName: body.DisplayName}
	if body.LegalName != nil {
		in.LegalName = *body.LegalName
	}
	if body.BillingEmail != nil {
		in.BillingEmail = *body.BillingEmail
	}
	sub, err := h.app.CreateSubAccount(ctx, callerFromContext(ctx), parentID, in)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateSubAccount201JSONResponse(tenantResponse(sub)), nil
}

func (h *Handlers) ListSubAccounts(ctx context.Context, request ListSubAccountsRequestObject) (ListSubAccountsResponseObject, error) {
	parentID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	subs, err := h.app.ListSubAccounts(ctx, callerFromContext(ctx), parentID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := SubAccountList{SubAccounts: make([]Tenant, 0, len(subs))}
	for _, sub := range subs {
		resp.SubAccounts = append(resp.SubAccounts, tenantResponse(sub))
	}
	return ListSubAccounts200JSONResponse(resp), nil
}

func tenantResponse(t *app.Tenant) Tenant {
	resp := Tenant{
		Id:              t.ID.String(),
		Type:            TenantType(t.Type),
		DisplayName:     t.DisplayName,
		CountryCode:     t.CountryCode,
		Timezone:        t.Timezone,
		DefaultCurrency: t.DefaultCurrency,
		Region:          t.Region,
		Tier:            TenantTier(t.Tier),
		Status:          TenantStatus(t.Status),
		CreatedAt:       t.CreatedAt,
	}
	if t.ParentTenantID != nil {
		parent := t.ParentTenantID.String()
		resp.ParentTenantId = &parent
	}
	if t.LegalName != "" {
		resp.LegalName = &t.LegalName
	}
	if t.BillingEmail != "" {
		resp.BillingEmail = &t.BillingEmail
	}
	return resp
}

// toProblem maps app/'s sentinels to a *problem.Problem; anything else
// passes through to respondProblem's SYS-9000.
func toProblem(err error) error {
	switch {
	case errors.Is(err, app.ErrValidation):
		return problem.New(problem.VAL4000, err.Error())
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "not found")
	case errors.Is(err, domain.ErrNotReseller):
		return problem.New(problem.KYC4021, "only a reseller account can create sub-accounts")
	case errors.Is(err, domain.ErrTenantNotActive):
		return problem.New(problem.KYC4021, "the account is suspended or archived")
	case errors.Is(err, domain.ErrResellerNeedsFinancial):
		return problem.New(problem.KYC4020, "reseller accounts need the Financial tier")
	default:
		return err
	}
}
