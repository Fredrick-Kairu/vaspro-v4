// Package grpc serves the ledger's internal posting API
// (proto/ledger/v1, task 3.3) to other VASPRO services. Never exposed
// to clients.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
)

// errorDomain is ErrorInfo.domain on every ledger error.
const errorDomain = "ledger.vaspro.io"

// Server implements ledgerv1.LedgerServiceServer over app.Service.
type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc *app.Service
	log *slog.Logger
}

// Register adds the ledger's gRPC service to s. log may be nil.
func Register(s *gogrpc.Server, svc *app.Service, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	ledgerv1.RegisterLedgerServiceServer(s, &Server{svc: svc, log: log})
}

// PostTransaction implements ledgerv1.LedgerServiceServer.
func (s *Server) PostTransaction(ctx context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	tenant, err := optionalTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	if req.GetPosting() == nil {
		return nil, invalid("a posting is required")
	}
	p, err := postRequest(tenant, req.GetPosting())
	if err != nil {
		return nil, err
	}
	res, err := s.svc.Post(ctx, p)
	if err != nil {
		return nil, s.toStatus(err)
	}
	out := &ledgerv1.PostTransactionResponse{TransactionId: res.TransactionID.String(), Replayed: res.Replayed}
	for _, l := range res.Lines {
		out.Lines = append(out.Lines, &ledgerv1.PostedLine{
			AccountId: l.AccountID.String(), Account: accountRef(l.Line.Account),
			Direction: string(rune(l.Line.Direction)), AmountMinor: l.Line.AmountMinor,
		})
	}
	return out, nil
}

// PurchaseTokens implements ledgerv1.LedgerServiceServer.
func (s *Server) PurchaseTokens(ctx context.Context, req *ledgerv1.PurchaseTokensRequest) (*ledgerv1.PurchaseTokensResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	tokens, err := parseTokens("tokens", req.GetTokens())
	if err != nil {
		return nil, err
	}
	createdBy, err := optionalUUID("created_by", req.GetCreatedBy())
	if err != nil {
		return nil, err
	}
	res, err := s.svc.PurchaseTokens(ctx, app.PurchaseRequest{
		TenantID: tenant, IdempotencyKey: req.GetIdempotencyKey(), ServiceCode: req.GetServiceCode(),
		Currency: req.GetCurrency(), Tokens: tokens, CreatedBy: createdBy,
	})
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.PurchaseTokensResponse{
		TransactionId: res.TransactionID.String(), LotId: res.LotID.String(), TokenPriceId: res.TokenPriceID.String(),
		MinorPerToken: res.MinorPerToken.String(), CostMinor: res.CostMinor, Replayed: res.Replayed,
	}, nil
}

// PlaceHold implements ledgerv1.LedgerServiceServer.
func (s *Server) PlaceHold(ctx context.Context, req *ledgerv1.PlaceHoldRequest) (*ledgerv1.PlaceHoldResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	ref, err := parseUUID("reference_id", req.GetReferenceId())
	if err != nil {
		return nil, err
	}
	tokens, err := optionalTokens("tokens", req.GetTokens())
	if err != nil {
		return nil, err
	}
	res, err := s.svc.PlaceHold(ctx, app.HoldRequest{
		TenantID: tenant, Account: accountKey(req.GetAccount()), ReferenceType: req.GetReferenceType(),
		ReferenceID: ref, AmountMinor: req.GetAmountMinor(), Tokens: tokens,
		ExpiresAt: time.Unix(req.GetExpiresAtUnix(), 0).UTC(),
	})
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.PlaceHoldResponse{Hold: holdMessage(res.Hold), Replayed: res.Replayed}, nil
}

// CaptureHold implements ledgerv1.LedgerServiceServer.
func (s *Server) CaptureHold(ctx context.Context, req *ledgerv1.CaptureHoldRequest) (*ledgerv1.CaptureHoldResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	ref, err := parseUUID("reference_id", req.GetReferenceId())
	if err != nil {
		return nil, err
	}
	capturedTokens, err := optionalTokens("captured_tokens", req.GetCapturedTokens())
	if err != nil {
		return nil, err
	}
	capture := app.CaptureRequest{
		TenantID: tenant, ReferenceType: req.GetReferenceType(), ReferenceID: ref,
		CapturedMinor: req.GetCapturedMinor(), CapturedTokens: capturedTokens, Events: req.GetEvents(),
	}
	if req.GetPosting() != nil {
		p, err := postRequest(&tenant, req.GetPosting())
		if err != nil {
			return nil, err
		}
		capture.Posting = &p
	}
	res, err := s.svc.CaptureHold(ctx, capture)
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.CaptureHoldResponse{Hold: holdMessage(res.Hold), Replayed: res.Replayed}, nil
}

// ReleaseHold implements ledgerv1.LedgerServiceServer.
func (s *Server) ReleaseHold(ctx context.Context, req *ledgerv1.ReleaseHoldRequest) (*ledgerv1.ReleaseHoldResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	ref, err := parseUUID("reference_id", req.GetReferenceId())
	if err != nil {
		return nil, err
	}
	res, err := s.svc.ReleaseHold(ctx, tenant, req.GetReferenceType(), ref)
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.ReleaseHoldResponse{Hold: holdMessage(res.Hold), Replayed: res.Replayed}, nil
}

// SetAutoBuyRule implements ledgerv1.LedgerServiceServer.
func (s *Server) SetAutoBuyRule(ctx context.Context, req *ledgerv1.SetAutoBuyRuleRequest) (*ledgerv1.SetAutoBuyRuleResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	below, err := decimal.NewFromString(req.GetBelowTokens())
	if err != nil {
		return nil, invalid("below_tokens must be a decimal")
	}
	buy, err := decimal.NewFromString(req.GetBuyTokens())
	if err != nil {
		return nil, invalid("buy_tokens must be a decimal")
	}
	rule, err := s.svc.SetAutoBuyRule(ctx, tenant, app.AutoBuyRule{
		ServiceCode: req.GetServiceCode(), Currency: req.GetCurrency(), BelowTokens: below, BuyTokens: buy, Enabled: req.GetEnabled(),
	})
	if err != nil {
		return nil, s.toStatus(err)
	}
	return &ledgerv1.SetAutoBuyRuleResponse{Rule: autoBuyRuleMessage(rule)}, nil
}

// ListAutoBuyRules implements ledgerv1.LedgerServiceServer.
func (s *Server) ListAutoBuyRules(ctx context.Context, req *ledgerv1.ListAutoBuyRulesRequest) (*ledgerv1.ListAutoBuyRulesResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	rules, err := s.svc.ListAutoBuyRules(ctx, tenant)
	if err != nil {
		return nil, s.toStatus(err)
	}
	out := &ledgerv1.ListAutoBuyRulesResponse{}
	for _, r := range rules {
		out.Rules = append(out.Rules, autoBuyRuleMessage(r))
	}
	return out, nil
}

// RecordPayback implements ledgerv1.LedgerServiceServer (task 3.7).
func (s *Server) RecordPayback(ctx context.Context, req *ledgerv1.RecordPaybackRequest) (*ledgerv1.RecordPaybackResponse, error) {
	tenant, err := requiredTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	id, err := parseUUID("reversal_id", req.GetReversalId())
	if err != nil {
		return nil, err
	}
	r, err := s.svc.RecordPayback(ctx, app.PaybackReport{
		TenantID: tenant, ReversalID: id, Succeeded: req.GetSucceeded(), Reference: req.GetReference(), Error: req.GetError(),
	})
	if err != nil {
		return nil, s.toStatus(err)
	}
	out := &ledgerv1.RecordPaybackResponse{Status: string(r.Status), PaybackAttempts: r.PaybackAttempts}
	if r.PaybackTransactionID != nil {
		out.PaybackTransactionId = r.PaybackTransactionID.String()
	}
	return out, nil
}

func autoBuyRuleMessage(r app.AutoBuyRule) *ledgerv1.AutoBuyRule {
	m := &ledgerv1.AutoBuyRule{
		RuleId: r.ID.String(), ServiceCode: r.ServiceCode, Currency: r.Currency,
		BelowTokens: r.BelowTokens.String(), BuyTokens: r.BuyTokens.String(), Enabled: r.Enabled,
	}
	if r.LastTriggeredAt != nil {
		m.LastTriggeredAtUnix = r.LastTriggeredAt.Unix()
	}
	return m
}

// GetBalances implements ledgerv1.LedgerServiceServer.
func (s *Server) GetBalances(ctx context.Context, req *ledgerv1.GetBalancesRequest) (*ledgerv1.GetBalancesResponse, error) {
	tenant, err := optionalTenant(req.GetTenantId())
	if err != nil {
		return nil, err
	}
	bals, err := s.svc.GetBalances(ctx, tenant)
	if err != nil {
		return nil, s.toStatus(err)
	}
	out := &ledgerv1.GetBalancesResponse{}
	for _, b := range bals {
		ab := &ledgerv1.AccountBalance{
			AccountId: b.AccountID.String(), Account: accountRef(b.Account), Status: b.Status,
			BalanceMinor: b.BalanceMinor, HeldMinor: b.HeldMinor, AvailableMinor: b.AvailableMinor,
		}
		if b.Account.Type == domain.ServiceWallet {
			ab.TokensRemaining, ab.HeldTokens, ab.TokensAvailable = b.TokensRemaining.String(), b.HeldTokens.String(), b.TokensAvailable.String()
		}
		out.Accounts = append(out.Accounts, ab)
	}
	return out, nil
}

// toStatus maps the app's errors to a gRPC status with the VASPRO code
// in an ErrorInfo detail. Anything unexpected is logged and INTERNAL
// with no detail of its cause.
func (s *Server) toStatus(err error) error {
	switch {
	case errors.Is(err, app.ErrValidation):
		return withCode(codes.InvalidArgument, problem.VAL4000, err.Error())
	case errors.Is(err, app.ErrNotFound):
		return withCode(codes.NotFound, problem.VAL4003, "not found")
	case errors.Is(err, app.ErrIdempotencyConflict):
		return withCode(codes.AlreadyExists, problem.VAL4001, err.Error())
	case errors.Is(err, app.ErrInsufficientBalance):
		return withCode(codes.FailedPrecondition, problem.BAL4001, err.Error())
	case errors.Is(err, app.ErrInsufficientTokens):
		return withCode(codes.FailedPrecondition, problem.BAL4002, err.Error())
	case errors.Is(err, app.ErrAccountNotOpen):
		return withCode(codes.FailedPrecondition, problem.BAL4006, err.Error())
	case errors.Is(err, domain.ErrHoldState):
		return withCode(codes.FailedPrecondition, problem.BAL4003, err.Error())
	case errors.Is(err, domain.ErrAllocationFinal):
		return withCode(codes.InvalidArgument, problem.BAL4004, err.Error())
	case errors.Is(err, app.ErrNoPrice):
		return withCode(codes.FailedPrecondition, problem.BAL4005, err.Error())
	case errors.Is(err, domain.ErrNotAwaitingPayback):
		return withCode(codes.FailedPrecondition, problem.VAL4016, err.Error())
	case errors.Is(err, domain.ErrReversalNotCovered):
		return withCode(codes.FailedPrecondition, problem.BAL4007, err.Error())
	}
	s.log.Error("ledger: request failed", "error", err)
	return withCode(codes.Internal, problem.SYS9000, "internal error")
}

func withCode(code codes.Code, vaspro problem.Code, msg string) error {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{Reason: string(vaspro), Domain: errorDomain})
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

func invalid(msg string) error { return withCode(codes.InvalidArgument, problem.VAL4000, msg) }

func parseUUID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, invalid(field + " must be a UUID")
	}
	return id, nil
}

func requiredTenant(s string) (uuid.UUID, error) { return parseUUID("tenant_id", s) }

// optionalTenant: empty means VASPRO's own side.
func optionalTenant(s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := parseUUID("tenant_id", s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func optionalUUID(field, s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := parseUUID(field, s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// parseTokens reads a required decimal token quantity.
func parseTokens(field, s string) (decimal.Decimal, error) {
	d, err := domain.ParseTokens(s)
	if err != nil {
		return decimal.Zero, invalid(field + " must be a positive decimal with at most 6 places")
	}
	return d, nil
}

// optionalTokens: empty means none.
func optionalTokens(field, s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	return parseTokens(field, s)
}

func postRequest(tenant *uuid.UUID, p *ledgerv1.Posting) (app.PostRequest, error) {
	refID, err := optionalUUID("reference_id", p.GetReferenceId())
	if err != nil {
		return app.PostRequest{}, err
	}
	createdBy, err := optionalUUID("created_by", p.GetCreatedBy())
	if err != nil {
		return app.PostRequest{}, err
	}
	req := app.PostRequest{
		TenantID: tenant, IdempotencyKey: p.GetIdempotencyKey(), Type: domain.TxnType(p.GetType()),
		Description: p.GetDescription(), ReferenceType: p.GetReferenceType(), ReferenceID: refID, CreatedBy: createdBy,
	}
	for i, l := range p.GetLines() {
		if len(l.GetDirection()) != 1 {
			return app.PostRequest{}, invalid(fmt.Sprintf("line %d: direction must be D or C", i))
		}
		req.Lines = append(req.Lines, domain.Line{
			Account: accountKey(l.GetAccount()), Direction: domain.Side(l.GetDirection()[0]), AmountMinor: l.GetAmountMinor(),
		})
	}
	return req, nil
}

func accountKey(a *ledgerv1.AccountRef) domain.AccountKey {
	return domain.AccountKey{Type: domain.AccountType(a.GetType()), ServiceCode: a.GetServiceCode(), Rail: a.GetRail(), Currency: a.GetCurrency()}
}

func accountRef(k domain.AccountKey) *ledgerv1.AccountRef {
	return &ledgerv1.AccountRef{Type: string(k.Type), ServiceCode: k.ServiceCode, Rail: k.Rail, Currency: k.Currency}
}

func holdMessage(h app.HoldView) *ledgerv1.Hold {
	m := &ledgerv1.Hold{
		HoldId: h.ID.String(), AccountId: h.AccountID.String(), ReferenceType: h.ReferenceType,
		ReferenceId: h.ReferenceID.String(), AmountMinor: h.AmountMinor, CapturedMinor: h.CapturedMinor,
		Status: string(h.Status), Settled: h.Settled,
	}
	if h.TransactionID != nil {
		m.TransactionId = h.TransactionID.String()
	}
	if !h.Tokens.IsZero() {
		m.Tokens = h.Tokens.String()
		if !h.CapturedTokens.IsZero() {
			m.CapturedTokens = h.CapturedTokens.String()
		}
	}
	return m
}
