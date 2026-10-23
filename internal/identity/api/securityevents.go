package api

import (
	"context"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

func (h *Handlers) ListSecurityEvents(ctx context.Context, request ListSecurityEventsRequestObject) (ListSecurityEventsResponseObject, error) {
	events, err := h.app.ListSecurityEvents(ctx, sessionUserIDFromContext(ctx))
	if err != nil {
		return nil, toProblem(err)
	}
	summaries := make([]SecurityEventSummary, len(events))
	for i, ev := range events {
		summaries[i] = securityEventSummaryResponse(&ev)
	}
	return ListSecurityEvents200JSONResponse{Events: &summaries}, nil
}

func securityEventSummaryResponse(ev *app.SecurityEvent) SecurityEventSummary {
	id := ev.ID.String()
	detail := map[string]any(ev.Detail)
	return SecurityEventSummary{
		Id:         &id,
		Type:       &ev.Type,
		Ip:         ev.IP,
		Detail:     &detail,
		OccurredAt: &ev.OccurredAt,
	}
}
