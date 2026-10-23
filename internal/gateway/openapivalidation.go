package gateway

import (
	"context"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// loadOpenAPISpec reads and validates specPath (docs/api/identity.yaml
// in production) — called once at startup, not per request.
func loadOpenAPISpec(specPath string) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("gateway: loading openapi spec %s: %w", specPath, err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("gateway: validating openapi spec %s: %w", specPath, err)
	}
	return spec, nil
}

// requireValidRequestShape validates every proxied request against
// spec before forwarding it — a bad request stops here instead of
// reaching identity (system design §8). identity's own routes already
// validate via oapi-codegen's strict typing; this is a second,
// edge-level check that would also protect a future backend that
// doesn't validate as strictly, and the one place "OpenAPI request
// validation" (this task's own line item) has a real spec to validate
// against today.
//
// Auth requirements in the spec (sessionBearer) are deliberately not
// checked here — openapi3filter.NoopAuthenticationFunc skips them —
// since that's identity's own requireSession's job (a dashboard
// session ID, not anything this middleware could verify) and the
// gateway's separate JWT pipeline's job for protected routes; this
// middleware only checks request shape. DoNotValidateServers avoids
// coupling to the spec's own `servers:` Host, which would otherwise
// reject every request whose Host doesn't match it.
func requireValidRequestShape(spec *openapi3.T) func(http.Handler) http.Handler {
	return nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		Options: openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
		ErrorHandlerWithOpts: func(_ context.Context, _ error, w http.ResponseWriter, r *http.Request, _ nethttpmiddleware.ErrorHandlerOpts) {
			problem.Respond(w, r, problem.New(problem.VAL4000, "the request doesn't match the API's OpenAPI spec"))
		},
	})
}
