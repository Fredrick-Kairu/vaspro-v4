package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// SpecOperation is one operation of docs/api/identity.yaml, as the
// spec-driven tests (this file's parity check and the api_test
// package's negative pass) need it. Exported from a _test.go file so
// both test packages share one loader; it isn't part of the package's
// real API.
type SpecOperation struct {
	OperationID   string
	Method        string
	Path          string
	HasBody       bool
	SessionBearer bool
}

// GoName is the name oapi-codegen gives the operation's Go method,
// which is what a StrictMiddlewareFunc is called with — not the
// spec's lowerCamelCase operationId (the 1.3 bug).
func (o SpecOperation) GoName() string {
	return strings.ToUpper(o.OperationID[:1]) + o.OperationID[1:]
}

// LoadSpecOperations returns every operation in docs/api/identity.yaml,
// so a test that ranges over them covers an endpoint the moment it's
// added to the contract.
func LoadSpecOperations(t testing.TB) []SpecOperation {
	t.Helper()
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/identity.yaml")
	if err != nil {
		t.Fatalf("loading docs/api/identity.yaml: %v", err)
	}
	var ops []SpecOperation
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			ops = append(ops, SpecOperation{
				OperationID:   op.OperationID,
				Method:        method,
				Path:          path,
				HasBody:       op.RequestBody != nil,
				SessionBearer: requiresSessionBearer(op),
			})
		}
	}
	slices.SortFunc(ops, func(a, b SpecOperation) int { return strings.Compare(a.OperationID, b.OperationID) })
	return ops
}

func requiresSessionBearer(op *openapi3.Operation) bool {
	if op.Security == nil {
		return false
	}
	for _, req := range *op.Security {
		if _, ok := req["sessionBearer"]; ok {
			return true
		}
	}
	return false
}

// TestRequireSessionOperations_MatchesSpec: the operations the contract
// marks `security: sessionBearer` are exactly the ones requireSession
// gates. A route missing from the map isn't rejected loudly — it runs
// with no session at all (the bugs found in 1.3 and 1.6) — so this
// check is what catches the next one.
func TestRequireSessionOperations_MatchesSpec(t *testing.T) {
	ops := LoadSpecOperations(t)
	inSpec := map[string]bool{}
	for _, op := range ops {
		inSpec[op.GoName()] = true
		if op.SessionBearer != requireSessionOperations[op.GoName()] {
			t.Errorf("%s: spec sessionBearer=%v, requireSessionOperations=%v",
				op.OperationID, op.SessionBearer, requireSessionOperations[op.GoName()])
		}
	}
	for name := range requireSessionOperations {
		if !inSpec[name] {
			t.Errorf("requireSessionOperations names %s, which isn't an operation in the spec", name)
		}
	}
	for name := range requireStepUpOperations {
		if !requireSessionOperations[name] {
			t.Errorf("requireStepUpOperations names %s, which requireSession doesn't gate (step-up reads the session it resolves)", name)
		}
	}
}
