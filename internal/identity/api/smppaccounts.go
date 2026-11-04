package api

import (
	"context"
	"math"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

func (h *Handlers) CreateSmppAccount(ctx context.Context, request CreateSmppAccountRequestObject) (CreateSmppAccountResponseObject, error) {
	tenantID, userID, err := h.sessionTenant(ctx, credentialsManage)
	if err != nil {
		return nil, toProblem(err)
	}
	body := request.Body
	// max_binds/max_tps come straight off the wire as plain JSON
	// integers (oapi-codegen's SmppAccountCreateRequest has no format
	// to narrow them) — bounds-checked here before narrowing to the
	// column widths (smallint/int), rather than truncating silently on
	// an out-of-range value.
	if body.MaxBinds <= 0 || body.MaxBinds > math.MaxInt16 {
		return nil, toProblem(app.ErrValidation)
	}
	if body.MaxTps <= 0 || body.MaxTps > math.MaxInt32 {
		return nil, toProblem(app.ErrValidation)
	}
	account, err := h.app.CreateSMPPAccount(
		ctx,
		tenantID, userID,
		body.SystemId,
		string(body.Environment),
		body.IpAllowlist,
		int16(body.MaxBinds),
		int32(body.MaxTps),
	)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateSmppAccount200JSONResponse(smppAccountWithPasswordResponse(account)), nil
}

func (h *Handlers) ListSmppAccounts(ctx context.Context, request ListSmppAccountsRequestObject) (ListSmppAccountsResponseObject, error) {
	tenantID, _, err := h.sessionTenant(ctx, credentialsRead)
	if err != nil {
		return nil, toProblem(err)
	}
	accounts, err := h.app.ListSMPPAccounts(ctx, tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	summaries := make([]SmppAccountSummary, len(accounts))
	for i, a := range accounts {
		summaries[i] = smppAccountSummaryResponse(&a)
	}
	return ListSmppAccounts200JSONResponse{Accounts: &summaries}, nil
}

func (h *Handlers) GetSmppAccount(ctx context.Context, request GetSmppAccountRequestObject) (GetSmppAccountResponseObject, error) {
	tenantID, _, err := h.sessionTenant(ctx, credentialsRead)
	if err != nil {
		return nil, toProblem(err)
	}
	accountID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	account, err := h.app.GetSMPPAccount(ctx, tenantID, accountID)
	if err != nil {
		return nil, toProblem(err)
	}
	return GetSmppAccount200JSONResponse(smppAccountSummaryResponse(account)), nil
}

func (h *Handlers) UpdateSmppAccountStatus(ctx context.Context, request UpdateSmppAccountStatusRequestObject) (UpdateSmppAccountStatusResponseObject, error) {
	tenantID, userID, err := h.sessionTenant(ctx, credentialsManage)
	if err != nil {
		return nil, toProblem(err)
	}
	accountID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	switch request.Body.Status {
	case Active:
		if err := h.app.ReactivateSMPPAccount(ctx, tenantID, userID, accountID); err != nil {
			return nil, toProblem(err)
		}
	case Suspended:
		if err := h.app.SuspendSMPPAccount(ctx, tenantID, userID, accountID); err != nil {
			return nil, toProblem(err)
		}
	default:
		return nil, toProblem(app.ErrValidation)
	}
	account, err := h.app.GetSMPPAccount(ctx, tenantID, accountID)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateSmppAccountStatus200JSONResponse(smppAccountSummaryResponse(account)), nil
}

func (h *Handlers) CreateSmppAccountPassword(ctx context.Context, request CreateSmppAccountPasswordRequestObject) (CreateSmppAccountPasswordResponseObject, error) {
	tenantID, userID, err := h.sessionTenant(ctx, credentialsManage)
	if err != nil {
		return nil, toProblem(err)
	}
	accountID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	account, err := h.app.RotateSMPPAccountPassword(ctx, tenantID, userID, accountID)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateSmppAccountPassword200JSONResponse(smppAccountWithPasswordResponse(account)), nil
}

func smppAccountSummaryResponse(a *app.SMPPAccountSummary) SmppAccountSummary {
	id := a.ID.String()
	environment := a.Environment
	status := a.Status
	maxBinds := int(a.MaxBinds)
	maxTPS := int(a.MaxTPS)
	return SmppAccountSummary{
		Id:          &id,
		SystemId:    &a.SystemID,
		IpAllowlist: &a.IPAllowlist,
		MaxBinds:    &maxBinds,
		MaxTps:      &maxTPS,
		Environment: &environment,
		Status:      &status,
	}
}

func smppAccountWithPasswordResponse(a *app.SMPPAccountWithPassword) SmppAccountWithPassword {
	summary := smppAccountSummaryResponse(&a.SMPPAccountSummary)
	return SmppAccountWithPassword{
		Id:          summary.Id,
		SystemId:    summary.SystemId,
		IpAllowlist: summary.IpAllowlist,
		MaxBinds:    summary.MaxBinds,
		MaxTps:      summary.MaxTps,
		Environment: summary.Environment,
		Status:      summary.Status,
		Password:    &a.Password,
	}
}
