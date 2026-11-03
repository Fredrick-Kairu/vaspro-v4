package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

// Staff operations (task 2.9, OQ-10). app/ checks the caller is staff
// on every call; a malformed ID is answered like an unknown one.

func (h *Handlers) ListKycReviews(ctx context.Context, request ListKycReviewsRequestObject) (ListKycReviewsResponseObject, error) {
	cursor, limit := "", 0
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	page, err := h.app.ListKYCReviews(ctx, callerFromContext(ctx), cursor, limit)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := ListKycReviews200JSONResponse{Reviews: make([]KycReviewSummary, len(page.Reviews))}
	for i, r := range page.Reviews {
		resp.Reviews[i] = kycReviewSummaryResponse(r)
	}
	if page.NextCursor != "" {
		resp.NextCursor = &page.NextCursor
	}
	return resp, nil
}

func (h *Handlers) GetKycReview(ctx context.Context, request GetKycReviewRequestObject) (GetKycReviewResponseObject, error) {
	checkID, err := uuid.Parse(request.CheckId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	review, err := h.app.GetKYCReview(ctx, callerFromContext(ctx), checkID)
	if err != nil {
		return nil, toProblem(err)
	}
	resp := GetKycReview200JSONResponse{Summary: kycReviewSummaryResponse(review.Summary), Check: kycCheckResponse(&review.Check)}
	if review.SubjectEmail != "" {
		resp.SubjectEmail = &review.SubjectEmail
	}
	if review.SubjectName != "" {
		resp.SubjectName = &review.SubjectName
	}
	if review.BusinessProfile != nil {
		profile := businessProfileResponse(review.BusinessProfile)
		resp.BusinessProfile = &profile
		docs := make([]Document, len(review.Documents))
		for i := range review.Documents {
			docs[i] = documentResponse(&review.Documents[i])
		}
		resp.Documents = &docs
	}
	return resp, nil
}

func (h *Handlers) DecideKycReview(ctx context.Context, request DecideKycReviewRequestObject) (DecideKycReviewResponseObject, error) {
	checkID, err := uuid.Parse(request.CheckId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	decision := domain.StaffDecision{Decision: string(request.Body.Decision), Reason: request.Body.Reason}
	if request.Body.RejectedDocumentIds != nil {
		for _, raw := range *request.Body.RejectedDocumentIds {
			id, err := uuid.Parse(raw)
			if err != nil {
				return nil, toProblem(domain.ErrInvalidDecision)
			}
			decision.RejectedDocumentIDs = append(decision.RejectedDocumentIDs, id)
		}
	}
	decided, err := h.app.DecideKYCReview(ctx, callerFromContext(ctx), checkID, decision)
	if err != nil {
		return nil, toProblem(err)
	}
	return DecideKycReview200JSONResponse(kycCheckResponse(decided)), nil
}

func (h *Handlers) CreateStaffTierChange(ctx context.Context, request CreateStaffTierChangeRequestObject) (CreateStaffTierChangeResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	var checkID *uuid.UUID
	if request.Body.KycCheckId != nil {
		id, err := uuid.Parse(*request.Body.KycCheckId)
		if err != nil {
			return nil, toProblem(app.ErrCheckNotUsable)
		}
		checkID = &id
	}
	tenant, err := h.app.StaffChangeTier(ctx, callerFromContext(ctx), tenantID, domain.Tier(request.Body.To), request.Body.Reason, checkID)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateStaffTierChange200JSONResponse(tenantResponse(tenant)), nil
}

func (h *Handlers) CreateStaffReseller(ctx context.Context, request CreateStaffResellerRequestObject) (CreateStaffResellerResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	tenant, err := h.app.StaffMakeReseller(ctx, callerFromContext(ctx), tenantID, request.Body.Reason)
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateStaffReseller200JSONResponse(tenantResponse(tenant)), nil
}

func (h *Handlers) GetStaffDocumentContent(ctx context.Context, request GetStaffDocumentContentRequestObject) (GetStaffDocumentContentResponseObject, error) {
	tenantID, err := uuid.Parse(request.Id)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	docID, err := uuid.Parse(request.DocId)
	if err != nil {
		return nil, toProblem(app.ErrNotFound)
	}
	data, mime, err := h.app.StaffDownloadDocument(ctx, callerFromContext(ctx), tenantID, docID)
	if err != nil {
		return nil, toProblem(err)
	}
	return staffDocumentContent{documentContent{data: data, mime: mime}}, nil
}

// staffDocumentContent serves a document to staff exactly as
// documentContent serves it to the account.
type staffDocumentContent struct{ documentContent }

func (d staffDocumentContent) VisitGetStaffDocumentContentResponse(w http.ResponseWriter) error {
	return d.VisitGetDocumentContentResponse(w)
}

func kycReviewSummaryResponse(r app.KYCReviewSummary) KycReviewSummary {
	resp := KycReviewSummary{
		CheckId: r.CheckID.String(), TenantId: r.TenantID.String(), AccountName: r.AccountName, CountryCode: r.CountryCode,
		Tier: r.Tier, TenantStatus: r.TenantStatus, Kind: KycReviewSummaryKind(r.Kind), CreatedAt: r.CreatedAt,
	}
	if r.Kind == domain.KYCKindBusiness {
		resp.DuplicateRegistration, resp.OtherAccounts = &r.DuplicateRegistration, &r.OtherAccounts
	}
	return resp
}
