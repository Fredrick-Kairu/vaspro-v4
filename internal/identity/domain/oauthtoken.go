package domain

import "strings"

// ErrScopeNotGranted means the caller asked for a scope the client
// doesn't have — api/'s oauth handler maps it to RFC 6749 §5.2's
// invalid_scope.
var ErrScopeNotGranted = errScopeNotGranted{}

type errScopeNotGranted struct{}

func (errScopeNotGranted) Error() string { return "domain: requested scope not granted to client" }

// GrantedScopes resolves the scopes an OAuth2 client-credentials token
// should carry: every scope the client has, if requested is empty
// (RFC 6749 §3.3 — omitting scope means "all of them"); otherwise
// requested's space-separated scopes, each of which must already be
// one of clientScopes, or ErrScopeNotGranted. The returned slice
// preserves clientScopes' own order rather than the caller's, so two
// requests asking for the same scopes in a different order still
// granted identically.
func GrantedScopes(clientScopes []string, requested string) ([]string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return clientScopes, nil
	}

	has := make(map[string]bool, len(clientScopes))
	for _, s := range clientScopes {
		has[s] = true
	}

	wanted := make(map[string]bool)
	for s := range strings.FieldsSeq(requested) {
		if !has[s] {
			return nil, ErrScopeNotGranted
		}
		wanted[s] = true
	}

	granted := make([]string, 0, len(wanted))
	for _, s := range clientScopes {
		if wanted[s] {
			granted = append(granted, s)
		}
	}
	return granted, nil
}
