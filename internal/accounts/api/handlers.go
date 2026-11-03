// Package api serves accounts' HTTP API (docs/api/accounts.yaml).
// Every request arrives through the gateway, which has already checked
// the dashboard session and forwarded the user (task 2.2a).
package api

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

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

func (h *Handlers) ListServices(ctx context.Context, request ListServicesRequestObject) (ListServicesResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	cat, err := h.app.ListServices(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := ServiceCatalogue{
		Tier:         ServiceCatalogueTier(cat.Tier),
		RequiredTier: ServiceCatalogueRequiredTier(cat.RequiredTier),
		Services:     make([]AccountService, len(cat.Services)),
	}
	for i := range cat.Services {
		resp.Services[i] = accountServiceResponse(&cat.Services[i])
	}
	return ListServices200JSONResponse(resp), nil
}

func (h *Handlers) SelectService(ctx context.Context, request SelectServiceRequestObject) (SelectServiceResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	svc, err := h.app.SelectService(ctx, callerFromContext(ctx), tenantID, request.Code)
	if err != nil {
		return nil, toProblem(err)
	}
	return SelectService200JSONResponse(accountServiceResponse(svc)), nil
}

func (h *Handlers) DeselectService(ctx context.Context, request DeselectServiceRequestObject) (DeselectServiceResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	if err := h.app.DeselectService(ctx, callerFromContext(ctx), tenantID, request.Code); err != nil {
		return nil, toProblem(err)
	}
	return DeselectService204Response{}, nil
}

func (h *Handlers) CreateKycCheck(ctx context.Context, request CreateKycCheckRequestObject) (CreateKycCheckResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	check, err := h.app.StartKYC(ctx, callerFromContext(ctx), tenantID, string(request.Body.Kind))
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateKycCheck201JSONResponse(kycCheckResponse(check)), nil
}

func (h *Handlers) ListKycChecks(ctx context.Context, request ListKycChecksRequestObject) (ListKycChecksResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	checks, err := h.app.ListKYCChecks(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := KycCheckList{Checks: make([]KycCheck, len(checks))}
	for i := range checks {
		resp.Checks[i] = kycCheckResponse(&checks[i])
	}
	return ListKycChecks200JSONResponse(resp), nil
}

func (h *Handlers) ReceiveKycWebhook(ctx context.Context, request ReceiveKycWebhookRequestObject) (ReceiveKycWebhookResponseObject, error) {
	wr, ok := webhookFromContext(ctx)
	if !ok {
		return nil, errors.New("api: the webhook body wasn't kept (keepWebhookBody isn't wired)")
	}
	if err := h.app.HandleKYCWebhook(ctx, request.Provider, wr.header, wr.body); err != nil {
		return nil, toProblem(err)
	}
	return ReceiveKycWebhook200Response{}, nil
}

func (h *Handlers) GetBusinessProfile(ctx context.Context, request GetBusinessProfileRequestObject) (GetBusinessProfileResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	p, err := h.app.GetBusinessProfile(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	return GetBusinessProfile200JSONResponse(businessProfileResponse(p)), nil
}

func (h *Handlers) SetBusinessProfile(ctx context.Context, request SetBusinessProfileRequestObject) (SetBusinessProfileResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	b := request.Body
	in := domain.BusinessProfileInput{RegistrationNumber: b.RegistrationNumber, RegisteredName: b.RegisteredName}
	if b.TaxId != nil {
		in.TaxID = *b.TaxId
	}
	if b.RegisteredAddress != nil {
		in.RegisteredAddress = *b.RegisteredAddress
	}
	p, err := h.app.SetBusinessProfile(ctx, callerFromContext(ctx), tenantID, in)
	if err != nil {
		return nil, toProblem(err)
	}
	return SetBusinessProfile200JSONResponse(businessProfileResponse(p)), nil
}

func (h *Handlers) AddBusinessOwner(ctx context.Context, request AddBusinessOwnerRequestObject) (AddBusinessOwnerResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	b := request.Body
	in := domain.OwnerInput{FullName: b.FullName, Role: string(b.Role)}
	if b.OwnershipPct != nil {
		in.OwnershipPct = *b.OwnershipPct
	}
	o, err := h.app.AddOwner(ctx, callerFromContext(ctx), tenantID, in)
	if err != nil {
		return nil, toProblem(err)
	}
	return AddBusinessOwner201JSONResponse(businessOwnerResponse(*o)), nil
}

func (h *Handlers) RemoveBusinessOwner(ctx context.Context, request RemoveBusinessOwnerRequestObject) (RemoveBusinessOwnerResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	ownerID, err := uuid.Parse(request.OwnerId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	if err := h.app.RemoveOwner(ctx, callerFromContext(ctx), tenantID, ownerID); err != nil {
		return nil, toProblem(err)
	}
	return RemoveBusinessOwner204Response{}, nil
}

func (h *Handlers) SubmitKyb(ctx context.Context, request SubmitKybRequestObject) (SubmitKybResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	check, err := h.app.SubmitKYB(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	return SubmitKyb201JSONResponse(kycCheckResponse(check)), nil
}

func (h *Handlers) UploadDocument(ctx context.Context, request UploadDocumentRequestObject) (UploadDocumentResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	docType, data, err := readUpload(request.Body)
	if err != nil {
		return nil, err
	}
	doc, created, err := h.app.UploadDocument(ctx, callerFromContext(ctx), tenantID, docType, data)
	if err != nil {
		return nil, toProblem(err)
	}
	if created {
		return UploadDocument201JSONResponse(documentResponse(doc)), nil
	}
	return UploadDocument200JSONResponse(documentResponse(doc)), nil
}

// readUpload reads the multipart form's type and file. A body over the
// route's limit (bodyLimit) is VAL-4008, as is a file part over 10 MB.
func readUpload(form *multipart.Reader) (docType string, data []byte, err error) {
	var sawFile bool
	for {
		part, err := form.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, uploadReadProblem(err)
		}
		switch part.FormName() {
		case "type":
			b, err := io.ReadAll(io.LimitReader(part, 64))
			if err != nil {
				return "", nil, uploadReadProblem(err)
			}
			docType = string(b)
		case "file":
			if data, err = io.ReadAll(io.LimitReader(part, domain.MaxDocumentBytes+1)); err != nil {
				return "", nil, uploadReadProblem(err)
			}
			sawFile = true
		}
	}
	if !sawFile || docType == "" {
		return "", nil, problem.New(problem.VAL4000, "a multipart form with type and file is required")
	}
	return docType, data, nil
}

func uploadReadProblem(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return problem.New(problem.VAL4008, "files are limited to 10 MB")
	}
	return problem.New(problem.VAL4000, "the upload could not be read")
}

func (h *Handlers) ListDocuments(ctx context.Context, request ListDocumentsRequestObject) (ListDocumentsResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	docs, err := h.app.ListDocuments(ctx, callerFromContext(ctx), tenantID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := DocumentList{Documents: make([]Document, len(docs))}
	for i := range docs {
		resp.Documents[i] = documentResponse(&docs[i])
	}
	return ListDocuments200JSONResponse(resp), nil
}

func (h *Handlers) GetDocumentContent(ctx context.Context, request GetDocumentContentRequestObject) (GetDocumentContentResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	docID, err := uuid.Parse(request.DocId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	data, mime, err := h.app.DownloadDocument(ctx, callerFromContext(ctx), tenantID, docID)
	if err != nil {
		return nil, toProblem(err)
	}
	return documentContent{data: data, mime: mime}, nil
}

// documentContent sends the file with its real type. It's served as an
// attachment with nosniff, so a browser never renders an upload inline.
type documentContent struct {
	data []byte
	mime string
}

func (d documentContent) VisitGetDocumentContentResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", d.mime)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(d.data)
	return err
}

func documentResponse(d *app.Document) Document {
	return Document{
		Id: d.ID.String(), Type: DocumentType(d.Type), MimeType: d.MIMEType, SizeBytes: int(d.SizeBytes),
		Sha256: d.SHA256, Status: DocumentStatus(d.Status), CreatedAt: d.CreatedAt,
	}
}

func businessProfileResponse(p *app.BusinessProfile) BusinessProfile {
	resp := BusinessProfile{
		Id: p.ID.String(), Registry: p.Registry, RegistrationNumber: p.RegistrationNumber, RegisteredName: p.RegisteredName,
		VerifiedAt: p.VerifiedAt, Owners: make([]BusinessOwner, len(p.Owners)),
	}
	if p.TaxID != "" {
		resp.TaxId = &p.TaxID
	}
	if p.RegisteredAddress != "" {
		resp.RegisteredAddress = &p.RegisteredAddress
	}
	for i, o := range p.Owners {
		resp.Owners[i] = businessOwnerResponse(o)
	}
	return resp
}

func businessOwnerResponse(o app.BusinessOwner) BusinessOwner {
	resp := BusinessOwner{Id: o.ID.String(), FullName: o.FullName, Role: BusinessOwnerRole(o.Role)}
	if o.OwnershipPct != "" {
		resp.OwnershipPct = &o.OwnershipPct
	}
	return resp
}

func kycCheckResponse(c *app.KYCCheck) KycCheck {
	resp := KycCheck{Id: c.ID.String(), Kind: KycCheckKind(c.Kind), Status: KycCheckStatus(c.Status), DecidedAt: c.DecidedAt, CreatedAt: c.CreatedAt}
	if c.URL != "" {
		resp.Url = &c.URL
	}
	return resp
}

func accountServiceResponse(s *app.AccountService) AccountService {
	resp := AccountService{Code: s.Code, Name: s.Name, MinTier: AccountServiceMinTier(s.MinTier), Selected: s.Selected, Locked: s.Locked}
	if s.Selected {
		status := AccountServiceStatus(s.Status)
		resp.Status = &status
	}
	return resp
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
	case errors.Is(err, app.ErrBadWebhook):
		return problem.New(problem.AUTH4013, "the webhook signature is invalid or expired")
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
	case errors.Is(err, domain.ErrKYBNeedsStarter):
		return problem.New(problem.KYC4020, "business verification needs the starter tier first")
	case errors.Is(err, domain.ErrNoBusinessProfile):
		return problem.New(problem.VAL4000, "enter the business profile first")
	case errors.Is(err, domain.ErrDocumentTypeNotAllowed):
		return problem.New(problem.VAL4007, "documents must be PDF, JPEG or PNG files")
	case errors.Is(err, domain.ErrDocumentTooLarge):
		return problem.New(problem.VAL4008, "files are limited to 10 MB")
	case errors.Is(err, app.ErrInfected):
		return problem.New(problem.VAL4009, "the virus scanner found something in the file; it wasn't stored")
	case errors.Is(err, domain.ErrUnknownDocumentType):
		return problem.New(problem.VAL4000, "unknown document type")
	case errors.Is(err, domain.ErrMissingDocuments):
		return problem.New(problem.VAL4000, strings.TrimPrefix(err.Error(), domain.ErrMissingDocuments.Error()+": "))
	case errors.Is(err, domain.ErrNoDirector):
		return problem.New(problem.VAL4000, "a business needs at least one director")
	case errors.Is(err, domain.ErrBusinessProfileLocked):
		return problem.New(problem.VAL4006, "the business profile is in review or verified")
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
