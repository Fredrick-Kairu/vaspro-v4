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

func (h *Handlers) ListRoles(ctx context.Context, _ ListRolesRequestObject) (ListRolesResponseObject, error) {
	roles, err := h.app.ListRoles(ctx)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := RoleList{Roles: make([]Role, len(roles))}
	for i, r := range roles {
		resp.Roles[i] = Role{Id: r.ID.String(), Name: r.Name, Permissions: r.Permissions}
	}
	return ListRoles200JSONResponse(resp), nil
}

func (h *Handlers) ListMyMemberships(ctx context.Context, _ ListMyMembershipsRequestObject) (ListMyMembershipsResponseObject, error) {
	mine, err := h.app.MyMemberships(ctx, callerFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	resp := MyMembershipList{Memberships: make([]MyMembership, len(mine))}
	for i, m := range mine {
		resp.Memberships[i] = MyMembership{
			TenantId: m.TenantID.String(), DisplayName: m.DisplayName,
			Type: MyMembershipType(m.Type), Tier: MyMembershipTier(m.Tier),
			RoleId: m.RoleID.String(), Status: MyMembershipStatus(m.Status),
		}
	}
	return ListMyMemberships200JSONResponse(resp), nil
}

func (h *Handlers) ListMembers(ctx context.Context, request ListMembersRequestObject) (ListMembersResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	members, err := h.app.ListMembers(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := MemberList{Members: make([]Member, len(members))}
	for i := range members {
		resp.Members[i] = memberResponse(&members[i])
	}
	return ListMembers200JSONResponse(resp), nil
}

func (h *Handlers) UpdateMember(ctx context.Context, request UpdateMemberRequestObject) (UpdateMemberResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	target, err := uuid.Parse(request.UserId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	roleID, err := uuid.Parse(request.Body.RoleId)
	if err != nil {
		return nil, toProblem(domain.ErrNotBuiltinRole)
	}
	m, err := h.app.ChangeMemberRole(ctx, callerFromContext(ctx), tenantID, target, roleID)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateMember200JSONResponse(memberResponse(m)), nil
}

func (h *Handlers) RemoveMember(ctx context.Context, request RemoveMemberRequestObject) (RemoveMemberResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	target, err := uuid.Parse(request.UserId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	if err := h.app.RemoveMember(ctx, callerFromContext(ctx), tenantID, target); err != nil {
		return nil, toProblem(err)
	}
	return RemoveMember204Response{}, nil
}

func (h *Handlers) CreateInvitation(ctx context.Context, request CreateInvitationRequestObject) (CreateInvitationResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	roleID, err := uuid.Parse(request.Body.RoleId)
	if err != nil {
		return nil, toProblem(domain.ErrNotBuiltinRole)
	}
	m, err := h.app.Invite(ctx, callerFromContext(ctx), sessionFromContext(ctx), tenantID, request.Body.Email, roleID)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateInvitation201JSONResponse(memberResponse(m)), nil
}

func (h *Handlers) AcceptInvitation(ctx context.Context, request AcceptInvitationRequestObject) (AcceptInvitationResponseObject, error) {
	tenantID, err := uuid.Parse(request.TenantId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	m, err := h.app.AcceptInvitation(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	return AcceptInvitation200JSONResponse(memberResponse(m)), nil
}

func memberResponse(m *app.Member) Member {
	resp := Member{UserId: m.UserID.String(), RoleId: m.RoleID.String(), Status: MemberStatus(m.Status), CreatedAt: m.CreatedAt}
	if m.Email != "" {
		resp.Email = &m.Email
	}
	if m.FullName != "" {
		resp.FullName = &m.FullName
	}
	if m.InvitedBy != nil {
		invitedBy := m.InvitedBy.String()
		resp.InvitedBy = &invitedBy
	}
	return resp
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
	case errors.Is(err, app.ErrStepUpRequired):
		return problem.New(problem.AUTH4012, "adding users needs a fresh step-up")
	case errors.Is(err, app.ErrAlreadyMember):
		return problem.New(problem.VAL4005, "already an active member of this account")
	case errors.Is(err, domain.ErrPermissionDenied):
		return problem.New(problem.AUTH4024, "your role in this account doesn't allow this")
	case errors.Is(err, domain.ErrLastAdmin):
		return problem.New(problem.VAL4004, "the account would have no active Admin")
	case errors.Is(err, domain.ErrNotBuiltinRole):
		return problem.New(problem.VAL4000, "only built-in roles can be assigned")
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
