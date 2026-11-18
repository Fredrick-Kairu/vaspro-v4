// Package grpc serves contacts' internal API (proto/contacts/v1, task
// 6.5) to other VASPRO services, messaging first. Never exposed to
// clients.
package grpc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	contactsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/contacts/v1"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
)

// Server implements contactsv1.ContactsServiceServer over app.Service.
type Server struct {
	contactsv1.UnimplementedContactsServiceServer
	svc *app.Service
	log *slog.Logger
}

// Register adds contacts' gRPC service to s. log may be nil.
func Register(s *gogrpc.Server, svc *app.Service, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	contactsv1.RegisterContactsServiceServer(s, &Server{svc: svc, log: log})
}

// CheckConsent implements contactsv1.ContactsServiceServer.
func (s *Server) CheckConsent(ctx context.Context, req *contactsv1.CheckConsentRequest) (*contactsv1.CheckConsentResponse, error) {
	tenant, err := tenantID(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	answers, err := s.svc.CheckConsent(ctx, tenant, req.GetChannel(), req.GetPurpose(), req.GetNumbers())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	out := &contactsv1.CheckConsentResponse{Answers: make([]*contactsv1.ConsentAnswer, len(answers))}
	for i, a := range answers {
		out.Answers[i] = &contactsv1.ConsentAnswer{Number: a.Number, Allowed: a.Allowed, Reason: a.Reason}
	}
	return out, nil
}

// ExpandAudience implements contactsv1.ContactsServiceServer: it sends
// the app's pages until one has no next cursor, marked last.
func (s *Server) ExpandAudience(req *contactsv1.ExpandAudienceRequest, stream gogrpc.ServerStreamingServer[contactsv1.ExpandAudienceResponse]) error {
	ctx := stream.Context()
	tenant, err := tenantID(req.GetTenantId())
	if err != nil {
		return err
	}
	in := app.AudienceRequest{Channel: req.GetChannel(), Purpose: req.GetPurpose(), PageSize: int(req.GetPageSize())}
	switch a := req.GetAudience().(type) {
	case *contactsv1.ExpandAudienceRequest_GroupId:
		id, err := uuid.Parse(a.GroupId)
		if err != nil {
			return status.Error(codes.InvalidArgument, "group_id isn't an ID")
		}
		in.GroupID = &id
	case *contactsv1.ExpandAudienceRequest_SegmentId:
		id, err := uuid.Parse(a.SegmentId)
		if err != nil {
			return status.Error(codes.InvalidArgument, "segment_id isn't an ID")
		}
		in.SegmentID = &id
	default:
		return status.Error(codes.InvalidArgument, "a group_id or segment_id is required")
	}
	for {
		page, err := s.svc.ExpandAudience(ctx, tenant, in)
		if err != nil {
			return s.toStatus(ctx, err)
		}
		out := &contactsv1.ExpandAudienceResponse{Blocked: page.Blocked, Last: page.Next == ""}
		for _, r := range page.Recipients {
			out.Recipients = append(out.Recipients, &contactsv1.Recipient{ContactId: r.ContactID.String(), Number: r.Number})
		}
		if err := stream.Send(out); err != nil {
			return err
		}
		if page.Next == "" {
			return nil
		}
		in.Cursor = page.Next
	}
}

// RecordReplyKeyword implements contactsv1.ContactsServiceServer.
func (s *Server) RecordReplyKeyword(ctx context.Context, req *contactsv1.RecordReplyKeywordRequest) (*contactsv1.RecordReplyKeywordResponse, error) {
	tenant, err := tenantID(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	matched, err := s.svc.RecordReplyKeyword(ctx, tenant, req.GetChannel(), req.GetNumber(), req.GetText())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &contactsv1.RecordReplyKeywordResponse{Matched: matched}, nil
}

// IssuePreferenceLink implements contactsv1.ContactsServiceServer.
func (s *Server) IssuePreferenceLink(ctx context.Context, req *contactsv1.IssuePreferenceLinkRequest) (*contactsv1.IssuePreferenceLinkResponse, error) {
	tenant, err := tenantID(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	link, err := s.svc.IssuePreferenceLinkFor(ctx, tenant, req.GetNumber())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &contactsv1.IssuePreferenceLinkResponse{Url: link.URL, ExpiresAtUnix: link.ExpiresAt.Unix()}, nil
}

func tenantID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, "tenant_id must be an account ID")
	}
	return id, nil
}

// toStatus maps the app's errors to gRPC codes; anything else is
// logged (without request data) and answered INTERNAL.
func (s *Server) toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, app.ErrValidation), errors.Is(err, app.ErrPhone):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, app.ErrFilter):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	s.log.ErrorContext(ctx, "contacts: gRPC call failed", "error", err)
	return status.Error(codes.Internal, "an unexpected error occurred")
}
