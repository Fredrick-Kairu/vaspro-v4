package gateway

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// TestAudit_LogsRouteTemplateNotRawPath: some paths carry a credential
// (a TOTP or passkey sign-in challenge), so the audit line names the
// OpenAPI route template, never the raw path.
func TestAudit_LogsRouteTemplateNotRawPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	spec, err := loadOpenAPISpec("../../docs/api/identity.yaml")
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}
	routes, err := gorillamux.NewRouter(spec)
	if err != nil {
		t.Fatalf("building route table: %v", err)
	}
	handler := audit(logger, routes)(okHandler())

	const challenge = "secret-challenge-token-value"
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/totp-challenges/"+challenge+"/verification", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if strings.Contains(line, challenge) {
		t.Fatalf("audit line contains the raw challenge token: %s", line)
	}
	if !strings.Contains(line, `"route":"/v1/auth/totp-challenges/{challenge}/verification"`) {
		t.Fatalf("audit line doesn't name the route template: %s", line)
	}
}
