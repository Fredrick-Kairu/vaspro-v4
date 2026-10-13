package problem_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// allCodes lists every generated constant, so the golden-output test below
// covers the whole catalogue without hand-copying each code into the test.
var allCodes = []problem.Code{
	problem.AUTH4010,
	problem.BAL4001,
	problem.CMP4030,
	problem.DLR7001,
	problem.KYC4020,
	problem.LIM4501,
	problem.PAY6501,
	problem.RATE4290,
	problem.RTE6020,
	problem.SYS5030,
	problem.SYS9000,
	problem.VAL4000,
}

func TestNew_GoldenOutputForEveryCode(t *testing.T) {
	for _, code := range allCodes {
		t.Run(string(code), func(t *testing.T) {
			p := problem.New(code, "a specific detail for this request")

			if p.Code != code {
				t.Errorf("Code = %q, want %q", p.Code, code)
			}
			if p.Type != "https://docs.vaspro.io/errors/"+string(code) {
				t.Errorf("Type = %q, missing or wrong docs URL", p.Type)
			}
			if p.Title == "" {
				t.Error("Title is empty")
			}
			if p.Status < 100 || p.Status > 599 {
				t.Errorf("Status = %d, not a valid HTTP status", p.Status)
			}
			if p.Detail != "a specific detail for this request" {
				t.Errorf("Detail = %q, want the detail New was given", p.Detail)
			}

			data, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("Problem did not marshal to JSON: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("marshaled Problem is not valid JSON: %v", err)
			}
			for _, field := range []string{"type", "title", "status", "code", "detail", "retryable"} {
				if _, ok := m[field]; !ok {
					t.Errorf("marshaled JSON is missing field %q: %s", field, data)
				}
			}
		})
	}
}

func TestNew_UnknownCodeFallsBackToSYS9000(t *testing.T) {
	bogus := problem.Code("NOT-A-REAL-CODE")

	p := problem.New(bogus, "original detail")

	if p.Code != problem.SYS9000 {
		t.Fatalf("expected an unknown code to fall back to SYS9000, got %q", p.Code)
	}
	if p.Status != 500 {
		t.Fatalf("expected status 500 for the SYS9000 fallback, got %d", p.Status)
	}
}

func TestProblem_Error(t *testing.T) {
	p := problem.New(problem.BAL4001, "KES wallet holds 120.00; this send needs 450.00.")

	var err error = p
	if err.Error() == "" {
		t.Fatal("expected a non-empty Error() string")
	}
}

func TestRespond_SetsContentTypeStatusAndRequestID(t *testing.T) {
	ctx := vaslog.WithRequestID(context.Background(), "req_abc123")
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	problem.Respond(w, r, problem.New(problem.BAL4001, "insufficient balance"))

	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	if w.Code != 402 {
		t.Fatalf("status = %d, want 402", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["request_id"] != "req_abc123" {
		t.Fatalf("request_id = %v, want %q", body["request_id"], "req_abc123")
	}
}

func TestRespond_NoRequestIDInContext_OmitsField(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()

	problem.Respond(w, r, problem.New(problem.VAL4000, "bad input"))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if _, ok := body["request_id"]; ok {
		t.Fatalf("expected no request_id field when none is in context, got: %v", body["request_id"])
	}
}
