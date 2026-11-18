package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// Handlers implements StrictServerInterface over app.Service.
type Handlers struct {
	app *app.Service
}

// NewHandlers builds Handlers.
func NewHandlers(a *app.Service) *Handlers { return &Handlers{app: a} }

var _ StrictServerInterface = (*Handlers)(nil)

func page(cursor *Cursor, limit *Limit) app.Page {
	p := app.Page{}
	if cursor != nil {
		p.Cursor = *cursor
	}
	if limit != nil {
		p.Limit = *limit
	}
	return p
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func nextCursor(s string) *string { return optional(s) }

func contactResponse(c app.Contact) Contact {
	attrs := make(map[string]interface{}, len(c.Attributes))
	for k, v := range c.Attributes {
		attrs[k] = v // json.RawMessage: numbers stay as their decimal text
	}
	return Contact{
		Id: c.ID.String(), Phone: c.Phone, FirstName: optional(c.FirstName), LastName: optional(c.LastName),
		Email: optional(c.Email), Attributes: attrs, Source: ContactSource(c.Source), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (h *Handlers) ListContacts(ctx context.Context, r ListContactsRequestObject) (ListContactsResponseObject, error) {
	var group *uuid.UUID
	if r.Params.GroupId != nil {
		id := parseOrNil(*r.Params.GroupId)
		group = &id
	}
	list, next, err := h.app.ListContacts(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), group, page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListContacts200JSONResponse{Contacts: make([]Contact, len(list)), NextCursor: nextCursor(next)}
	for i, c := range list {
		out.Contacts[i] = contactResponse(c)
	}
	return out, nil
}

func (h *Handlers) CreateContact(ctx context.Context, r CreateContactRequestObject) (CreateContactResponseObject, error) {
	b := r.Body
	in := app.ContactInput{Phone: b.Phone, Fields: domain.ContactFields{FirstName: deref(b.FirstName), LastName: deref(b.LastName), Email: deref(b.Email)}}
	if b.Attributes != nil {
		in.Attributes = *b.Attributes
	}
	c, created, err := h.app.CreateContact(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), in)
	if err != nil {
		return nil, toProblem(err)
	}
	if created {
		return CreateContact201JSONResponse(contactResponse(c)), nil
	}
	return CreateContact200JSONResponse(contactResponse(c)), nil
}

func (h *Handlers) GetContact(ctx context.Context, r GetContactRequestObject) (GetContactResponseObject, error) {
	c, err := h.app.GetContact(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ContactId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetContact200JSONResponse(contactResponse(c)), nil
}

func (h *Handlers) UpdateContact(ctx context.Context, r UpdateContactRequestObject) (UpdateContactResponseObject, error) {
	b := r.Body
	p := app.ContactPatch{FirstName: b.FirstName, LastName: b.LastName, Email: b.Email}
	if b.Attributes != nil {
		p.Attributes = *b.Attributes
	}
	c, err := h.app.UpdateContact(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ContactId), p)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateContact200JSONResponse(contactResponse(c)), nil
}

func (h *Handlers) DeleteContact(ctx context.Context, r DeleteContactRequestObject) (DeleteContactResponseObject, error) {
	if err := h.app.DeleteContact(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ContactId)); err != nil {
		return nil, toProblem(err)
	}
	return DeleteContact204Response{}, nil
}

func attributeResponse(d app.AttributeDef) AttributeDefinition {
	return AttributeDefinition{Key: d.Key, Label: d.Label, Type: AttributeDefinitionType(d.Type), CreatedAt: d.CreatedAt}
}

func (h *Handlers) ListAttributeDefinitions(ctx context.Context, r ListAttributeDefinitionsRequestObject) (ListAttributeDefinitionsResponseObject, error) {
	defs, err := h.app.ListAttributes(ctx, callerFromContext(ctx), parseOrNil(r.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListAttributeDefinitions200JSONResponse{Attributes: make([]AttributeDefinition, len(defs))}
	for i, d := range defs {
		out.Attributes[i] = attributeResponse(d)
	}
	return out, nil
}

func (h *Handlers) CreateAttributeDefinition(ctx context.Context, r CreateAttributeDefinitionRequestObject) (CreateAttributeDefinitionResponseObject, error) {
	d, err := h.app.CreateAttribute(ctx, callerFromContext(ctx), parseOrNil(r.TenantId),
		domain.AttributeDef{Key: r.Body.Key, Label: r.Body.Label, Type: string(r.Body.Type)})
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateAttributeDefinition201JSONResponse(attributeResponse(d)), nil
}

func (h *Handlers) DeleteAttributeDefinition(ctx context.Context, r DeleteAttributeDefinitionRequestObject) (DeleteAttributeDefinitionResponseObject, error) {
	if err := h.app.DeleteAttribute(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.AttributeKey); err != nil {
		return nil, toProblem(err)
	}
	return DeleteAttributeDefinition204Response{}, nil
}

func groupResponse(g app.Group) Group {
	return Group{Id: g.ID.String(), Name: g.Name, Description: optional(g.Description), Members: g.Members, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
}

func (h *Handlers) ListGroups(ctx context.Context, r ListGroupsRequestObject) (ListGroupsResponseObject, error) {
	groups, err := h.app.ListGroups(ctx, callerFromContext(ctx), parseOrNil(r.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListGroups200JSONResponse{Groups: make([]Group, len(groups))}
	for i, g := range groups {
		out.Groups[i] = groupResponse(g)
	}
	return out, nil
}

func (h *Handlers) CreateGroup(ctx context.Context, r CreateGroupRequestObject) (CreateGroupResponseObject, error) {
	g, err := h.app.CreateGroup(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.Body.Name, deref(r.Body.Description))
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateGroup201JSONResponse(groupResponse(g)), nil
}

func (h *Handlers) GetGroup(ctx context.Context, r GetGroupRequestObject) (GetGroupResponseObject, error) {
	g, err := h.app.GetGroup(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.GroupId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetGroup200JSONResponse(groupResponse(g)), nil
}

func (h *Handlers) UpdateGroup(ctx context.Context, r UpdateGroupRequestObject) (UpdateGroupResponseObject, error) {
	g, err := h.app.UpdateGroup(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.GroupId), r.Body.Name, r.Body.Description)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateGroup200JSONResponse(groupResponse(g)), nil
}

func (h *Handlers) DeleteGroup(ctx context.Context, r DeleteGroupRequestObject) (DeleteGroupResponseObject, error) {
	if err := h.app.DeleteGroup(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.GroupId)); err != nil {
		return nil, toProblem(err)
	}
	return DeleteGroup204Response{}, nil
}

func (h *Handlers) ListGroupMembers(ctx context.Context, r ListGroupMembersRequestObject) (ListGroupMembersResponseObject, error) {
	group := parseOrNil(r.GroupId)
	list, next, err := h.app.ListContacts(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), &group, page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListGroupMembers200JSONResponse{Contacts: make([]Contact, len(list)), NextCursor: nextCursor(next)}
	for i, c := range list {
		out.Contacts[i] = contactResponse(c)
	}
	return out, nil
}

func (h *Handlers) AddGroupMembers(ctx context.Context, r AddGroupMembersRequestObject) (AddGroupMembersResponseObject, error) {
	ids := make([]uuid.UUID, len(r.Body.ContactIds))
	for i, s := range r.Body.ContactIds {
		ids[i] = parseOrNil(s)
	}
	n, err := h.app.AddGroupMembers(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.GroupId), ids)
	if err != nil {
		return nil, toProblem(err)
	}
	return AddGroupMembers200JSONResponse{Added: n}, nil
}

func (h *Handlers) RemoveGroupMember(ctx context.Context, r RemoveGroupMemberRequestObject) (RemoveGroupMemberResponseObject, error) {
	if err := h.app.RemoveGroupMember(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.GroupId), parseOrNil(r.ContactId)); err != nil {
		return nil, toProblem(err)
	}
	return RemoveGroupMember204Response{}, nil
}

func importResponse(i app.Import) Import {
	out := Import{
		Id: i.ID.String(), Status: ImportStatus(i.Status), FileName: i.FileName, ContentType: i.ContentType,
		SizeBytes: i.SizeBytes, RowsTotal: i.RowsTotal, RowsValid: i.RowsValid, RowsFailed: i.RowsFailed,
		UploadExpiresAt: i.UploadExpiresAt, FinishedAt: i.FinishedAt, DeleteAfter: i.DeleteAfter, DeletedAt: i.DeletedAt,
		HasErrorReport: i.HasErrorReport, CreatedAt: i.CreatedAt,
	}
	if i.GroupID != nil {
		g := i.GroupID.String()
		out.GroupId = &g
	}
	return out
}

func (h *Handlers) ListImports(ctx context.Context, r ListImportsRequestObject) (ListImportsResponseObject, error) {
	list, next, err := h.app.ListImports(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListImports200JSONResponse{Imports: make([]Import, len(list)), NextCursor: nextCursor(next)}
	for i, imp := range list {
		out.Imports[i] = importResponse(imp)
	}
	return out, nil
}

func (h *Handlers) CreateImport(ctx context.Context, r CreateImportRequestObject) (CreateImportResponseObject, error) {
	in := app.ImportInput{FileName: r.Body.FileName, ContentType: string(r.Body.ContentType)}
	if r.Body.GroupId != nil {
		g := parseOrNil(*r.Body.GroupId)
		in.GroupID = &g
	}
	imp, up, err := h.app.CreateImport(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), in)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateImport201JSONResponse{
		Import: importResponse(imp),
		Upload: UploadForm{Url: up.Form.URL, Fields: up.Form.Fields, ExpiresAt: up.ExpiresAt, MaxBytes: up.MaxBytes},
	}, nil
}

func (h *Handlers) GetImport(ctx context.Context, r GetImportRequestObject) (GetImportResponseObject, error) {
	imp, err := h.app.GetImport(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ImportId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetImport200JSONResponse(importResponse(imp)), nil
}

func (h *Handlers) ConfirmImportUpload(ctx context.Context, r ConfirmImportUploadRequestObject) (ConfirmImportUploadResponseObject, error) {
	imp, err := h.app.ConfirmImportUpload(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ImportId))
	if err != nil {
		return nil, toProblem(err)
	}
	return ConfirmImportUpload202JSONResponse(importResponse(imp)), nil
}

func (h *Handlers) ListImportErrors(ctx context.Context, r ListImportErrorsRequestObject) (ListImportErrorsResponseObject, error) {
	errs, next, err := h.app.ListImportErrors(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ImportId), page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListImportErrors200JSONResponse{Errors: make([]ImportError, len(errs)), NextCursor: nextCursor(next)}
	for i, e := range errs {
		out.Errors[i] = ImportError{Row: e.Row, Field: e.Field, Code: e.Code, Message: e.Msg}
	}
	return out, nil
}

func (h *Handlers) CreateImportDownloadLink(ctx context.Context, r CreateImportDownloadLinkRequestObject) (CreateImportDownloadLinkResponseObject, error) {
	url, expires, err := h.app.CreateDownloadLink(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.ImportId), r.Body.File == Errors)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateImportDownloadLink201JSONResponse{Url: url, ExpiresAt: expires}, nil
}

func (h *Handlers) DownloadImportFile(ctx context.Context, r DownloadImportFileRequestObject) (DownloadImportFileResponseObject, error) {
	d, err := h.app.OpenDownload(ctx, r.Token)
	if err != nil {
		return nil, toProblem(err)
	}
	return fileResponse{d}, nil
}

// fileResponse streams a downloaded file as an attachment the browser
// saves, never renders, and doesn't keep; it closes the file after.
type fileResponse struct{ app.Download }

func (f fileResponse) VisitDownloadImportFileResponse(w http.ResponseWriter) error {
	defer func() { _ = f.Body.Close() }()
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.FileName}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(w, f.Body)
	return err
}

func segmentResponse(s app.Segment) Segment {
	return Segment{Id: s.ID.String(), Name: s.Name, Filter: s.Filter, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func (h *Handlers) ListSegments(ctx context.Context, r ListSegmentsRequestObject) (ListSegmentsResponseObject, error) {
	list, err := h.app.ListSegments(ctx, callerFromContext(ctx), parseOrNil(r.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListSegments200JSONResponse{Segments: make([]Segment, len(list))}
	for i, s := range list {
		out.Segments[i] = segmentResponse(s)
	}
	return out, nil
}

func (h *Handlers) CreateSegment(ctx context.Context, r CreateSegmentRequestObject) (CreateSegmentResponseObject, error) {
	s, err := h.app.CreateSegment(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.Body.Name, r.Body.Filter)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateSegment201JSONResponse(segmentResponse(s)), nil
}

func (h *Handlers) GetSegment(ctx context.Context, r GetSegmentRequestObject) (GetSegmentResponseObject, error) {
	s, err := h.app.GetSegment(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.SegmentId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetSegment200JSONResponse(segmentResponse(s)), nil
}

func (h *Handlers) UpdateSegment(ctx context.Context, r UpdateSegmentRequestObject) (UpdateSegmentResponseObject, error) {
	var filter json.RawMessage
	if r.Body.Filter != nil {
		filter = *r.Body.Filter
	}
	s, err := h.app.UpdateSegment(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.SegmentId), r.Body.Name, filter)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdateSegment200JSONResponse(segmentResponse(s)), nil
}

func (h *Handlers) DeleteSegment(ctx context.Context, r DeleteSegmentRequestObject) (DeleteSegmentResponseObject, error) {
	if err := h.app.DeleteSegment(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.SegmentId)); err != nil {
		return nil, toProblem(err)
	}
	return DeleteSegment204Response{}, nil
}

func (h *Handlers) GetSegmentCount(ctx context.Context, r GetSegmentCountRequestObject) (GetSegmentCountResponseObject, error) {
	n, err := h.app.CountSegment(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.SegmentId))
	if err != nil {
		return nil, toProblem(err)
	}
	return GetSegmentCount200JSONResponse{Count: n}, nil
}

func (h *Handlers) ListSegmentContacts(ctx context.Context, r ListSegmentContactsRequestObject) (ListSegmentContactsResponseObject, error) {
	list, next, err := h.app.ListSegmentContacts(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), parseOrNil(r.SegmentId), page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListSegmentContacts200JSONResponse{Contacts: make([]Contact, len(list)), NextCursor: nextCursor(next)}
	for i, c := range list {
		out.Contacts[i] = contactResponse(c)
	}
	return out, nil
}

func consentResponse(c app.Consent) Consent {
	return Consent{
		Channel: ConsentChannel(c.Channel), Purpose: ConsentPurpose(c.Purpose), Status: ConsentStatus(c.Status),
		Actor: ConsentActor(c.Actor), Source: ConsentSource(c.Source), Evidence: optional(c.Evidence), RecordedAt: c.RecordedAt,
	}
}

func suppressionResponse(sp app.Suppression) Suppression {
	out := Suppression{Phone: sp.Phone, Reason: SuppressionReason(sp.Reason), Note: optional(sp.Note), CreatedAt: sp.CreatedAt}
	if sp.Channel != "" {
		ch := SuppressionChannel(sp.Channel)
		out.Channel = &ch
	}
	return out
}

func (h *Handlers) RecordConsent(ctx context.Context, r RecordConsentRequestObject) (RecordConsentResponseObject, error) {
	c, err := h.app.RecordConsent(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), app.ConsentInput{
		Phone: r.Body.Phone, Channel: string(r.Body.Channel), Purpose: string(r.Body.Purpose), Status: string(r.Body.Status),
		Evidence: deref(r.Body.Evidence),
	})
	if err != nil {
		return nil, toProblem(err)
	}
	return RecordConsent200JSONResponse(consentResponse(c)), nil
}

func (h *Handlers) LookupConsent(ctx context.Context, r LookupConsentRequestObject) (LookupConsentResponseObject, error) {
	st, err := h.app.LookupConsent(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.Body.Phone)
	if err != nil {
		return nil, toProblem(err)
	}
	out := LookupConsent200JSONResponse{
		Phone: st.Phone, Consents: []Consent{}, Suppressions: []Suppression{}, Decisions: []ConsentDecision{}, History: []ConsentChange{},
	}
	if st.ContactID != nil {
		out.ContactId = optional(st.ContactID.String())
	}
	for _, c := range st.Consents {
		out.Consents = append(out.Consents, consentResponse(c))
	}
	for _, sp := range st.Suppressions {
		out.Suppressions = append(out.Suppressions, suppressionResponse(sp))
	}
	for _, d := range st.Decisions {
		cd := ConsentDecision{Channel: ConsentDecisionChannel(d.Channel), Purpose: ConsentDecisionPurpose(d.Purpose), Allowed: d.Allowed}
		if d.Reason != "" {
			reason := ConsentDecisionReason(d.Reason)
			cd.Reason = &reason
		}
		out.Decisions = append(out.Decisions, cd)
	}
	for _, e := range st.History {
		ch := ConsentChange{
			Channel: ConsentChangeChannel(e.Channel), Purpose: ConsentChangePurpose(e.Purpose), Status: e.Status, Actor: e.Actor,
			Source: e.Source, Evidence: optional(e.Evidence), OccurredAt: e.OccurredAt,
		}
		if e.RecordedBy != nil {
			ch.RecordedBy = optional(e.RecordedBy.String())
		}
		out.History = append(out.History, ch)
	}
	return out, nil
}

func (h *Handlers) ListSuppressions(ctx context.Context, r ListSuppressionsRequestObject) (ListSuppressionsResponseObject, error) {
	list, next, err := h.app.ListSuppressions(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), page(r.Params.Cursor, r.Params.Limit))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListSuppressions200JSONResponse{Suppressions: make([]Suppression, len(list)), NextCursor: nextCursor(next)}
	for i, sp := range list {
		out.Suppressions[i] = suppressionResponse(sp)
	}
	return out, nil
}

func (h *Handlers) AddSuppression(ctx context.Context, r AddSuppressionRequestObject) (AddSuppressionResponseObject, error) {
	sp, created, err := h.app.AddSuppression(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), app.SuppressionInput{
		Phone: r.Body.Phone, Channel: deref(r.Body.Channel), Reason: string(r.Body.Reason), Note: deref(r.Body.Note),
	})
	if err != nil {
		return nil, toProblem(err)
	}
	if created {
		return AddSuppression201JSONResponse(suppressionResponse(sp)), nil
	}
	return AddSuppression200JSONResponse(suppressionResponse(sp)), nil
}

func (h *Handlers) RemoveSuppression(ctx context.Context, r RemoveSuppressionRequestObject) (RemoveSuppressionResponseObject, error) {
	if err := h.app.RemoveSuppression(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.Body.Phone, deref(r.Body.Channel)); err != nil {
		return nil, toProblem(err)
	}
	return RemoveSuppression204Response{}, nil
}

func (h *Handlers) IssuePreferenceLink(ctx context.Context, r IssuePreferenceLinkRequestObject) (IssuePreferenceLinkResponseObject, error) {
	l, err := h.app.IssuePreferenceLink(ctx, callerFromContext(ctx), parseOrNil(r.TenantId), r.Body.Phone)
	if err != nil {
		return nil, toProblem(err)
	}
	return IssuePreferenceLink201JSONResponse{Url: l.URL, ExpiresAt: l.ExpiresAt}, nil
}

func preferencesResponse(p app.Preferences) Preferences {
	out := Preferences{MaskedPhone: p.MaskedPhone, ExpiresAt: p.ExpiresAt, Channels: make([]ChannelPreference, len(p.Channels))}
	for i, c := range p.Channels {
		out.Channels[i] = ChannelPreference{Channel: ChannelPreferenceChannel(c.Channel), Receiving: c.Receiving}
	}
	return out
}

func (h *Handlers) GetPreferences(ctx context.Context, r GetPreferencesRequestObject) (GetPreferencesResponseObject, error) {
	p, err := h.app.GetPreferences(ctx, r.Token)
	if err != nil {
		return nil, toProblem(err)
	}
	return GetPreferences200JSONResponse(preferencesResponse(p)), nil
}

func (h *Handlers) UpdatePreferences(ctx context.Context, r UpdatePreferencesRequestObject) (UpdatePreferencesResponseObject, error) {
	choice := map[string]bool{}
	if r.Body.Channels.Sms != nil {
		choice["sms"] = *r.Body.Channels.Sms
	}
	if r.Body.Channels.Whatsapp != nil {
		choice["whatsapp"] = *r.Body.Channels.Whatsapp
	}
	p, err := h.app.UpdatePreferences(ctx, r.Token, choice)
	if err != nil {
		return nil, toProblem(err)
	}
	return UpdatePreferences200JSONResponse(preferencesResponse(p)), nil
}

func toProblem(err error) error {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return problem.New(problem.VAL4003, "not found")
	case errors.Is(err, app.ErrPermissionDenied):
		return problem.New(problem.AUTH4024, "your role in this account doesn't allow this")
	case errors.Is(err, app.ErrPhone):
		return problem.New(problem.VAL4020, err.Error())
	case errors.Is(err, app.ErrValue):
		return problem.New(problem.VAL4021, err.Error())
	case errors.Is(err, app.ErrImportState):
		return problem.New(problem.VAL4023, "the import isn't awaiting its upload, or its upload form has expired")
	case errors.Is(err, app.ErrFileDeleted):
		return problem.New(problem.VAL4024, "the file has been deleted: files go 2 days after an import finishes")
	case errors.Is(err, app.ErrSubjectWithdrew):
		return problem.New(problem.VAL4026, "the person withdrew this consent themselves; only they can give it back")
	case errors.Is(err, app.ErrFilter):
		return problem.New(problem.VAL4025, strings.TrimPrefix(err.Error(), app.ErrFilter.Error()+": "))
	case errors.Is(err, app.ErrValidation):
		return problem.New(problem.VAL4000, err.Error())
	}
	return err
}

// parseOrNil reads a path ID; one that isn't a UUID names nothing, and
// uuid.Nil goes on so the access check answers it as not found.
func parseOrNil(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}
