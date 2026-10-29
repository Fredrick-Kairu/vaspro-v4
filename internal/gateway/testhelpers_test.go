package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// decodeJSON decodes rec's body into v — shared by every unit test in
// this package that asserts on a problem+json response shape.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
}
