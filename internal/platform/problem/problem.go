// Package problem writes RFC 9457 problem+json error responses using the
// code families generated from docs/errors/catalogue.yaml (see
// codes_gen.go, tools/errcodes).
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
)

const docsBaseURL = "https://docs.vaspro.io/errors/"

// Problem is a VASPRO error response (system design §8). Stack traces and
// internal messages never belong in Detail — they go to logs instead,
// under the same request ID.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      Code   `json:"code"`
	Detail    string `json:"detail"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id,omitempty"`
}

func (p *Problem) Error() string {
	return fmt.Sprintf("%s: %s", p.Code, p.Detail)
}

// New builds a Problem from a catalogue Code and a request-specific detail
// message. An unrecognized code (only possible by constructing a Code by
// hand rather than using a generated constant) falls back to SYS9000
// rather than panicking — this never runs in a request path that can't
// afford to.
func New(code Code, detail string) *Problem {
	e, ok := registry[code]
	if !ok {
		code = SYS9000
		e = registry[SYS9000]
		detail = fmt.Sprintf("used unknown error code; original detail: %s", detail)
	}
	return &Problem{
		Type:      docsBaseURL + string(code),
		Title:     e.Title,
		Status:    e.Status,
		Code:      code,
		Detail:    detail,
		Retryable: e.Retryable,
	}
}

// Respond writes p as the HTTP response: application/problem+json,
// p.Status, and p's JSON body with RequestID filled from r's context (if
// platform/log's request-ID middleware set one).
func Respond(w http.ResponseWriter, r *http.Request, p *Problem) {
	if id, ok := vaslog.RequestID(r.Context()); ok {
		p.RequestID = id
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
