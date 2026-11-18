package domain

import "slices"

// ClientEventTypes are the events sent to clients: the schemas in
// docs/events/ marked "x-vaspro-webhook": true (OQ-23, data model §8).
// A contract test keeps this list equal to the markers.
var ClientEventTypes = []string{
	"autobuy.triggered",
	"collection_notice.matched",
	"consent.withdrawn",
	"contact_import.finished",
	"kyc.check_decided",
	"membership.invited",
	"membership.joined",
	"membership.removed",
	"membership.role_changed",
	"payment_intent.failed",
	"payment_intent.succeeded",
	"reversal.paid_back",
	"reversal.posted",
	"service.deselected",
	"service.enabled",
	"service.locked",
	"service.selected",
	"tenant.created",
	"tenant.tier_changed",
	"tenant.type_changed",
	"tokens.purchased",
	"usage.posted",
	"wallet.credited",
	"webhook_endpoint.disabled",
}

// IsClientEventType reports whether t is sent to clients.
func IsClientEventType(t string) bool { return slices.Contains(ClientEventTypes, t) }
