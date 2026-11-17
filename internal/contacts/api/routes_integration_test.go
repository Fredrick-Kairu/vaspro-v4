//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/contactaccess"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
)

type harness struct {
	srv    *httptest.Server
	svc    *app.Service
	access *contactaccess.Fake
	clk    *clock.Fake
}

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/contacts"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE contacts_app LOGIN PASSWORD 'contacts_app'; GRANT contacts_service TO contacts_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("contacts_app", "contacts_app")+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := testkit.StartObjectStorage(t)
	cfg.Bucket, cfg.PublicEndpoint = "contacts", cfg.Endpoint
	store, err := objectstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	scanner, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scanner.Close)
	kms, _ := vascrypto.NewLocalKMS(map[string][]byte{"contacts-files": bytes.Repeat([]byte{5}, 32)})
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	upCfg := uploads.DefaultConfig()
	upCfg.KMSKeyID, upCfg.DownloadKey = "contacts-files", bytes.Repeat([]byte{6}, 32)
	up, err := uploads.New(store, clamav.New(scanner.Addr()), vascrypto.NewEnvelope(kms), clk, upCfg)
	if err != nil {
		t.Fatal(err)
	}
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	access := contactaccess.New()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := app.New(app.Deps{
		Pool: pool, Clock: clk, Logger: quiet, Accounts: access, Packs: map[string]*packs.Pack{"KE": ke},
		Uploads: up, DownloadBaseURL: "https://api.vaspro.test",
	})
	srv := httptest.NewServer(api.Routes(api.NewHandlers(svc), quiet, nil, idempotency.NewPostgresStore(pool)))
	t.Cleanup(srv.Close)
	return harness{srv: srv, svc: svc, access: access, clk: clk}
}

func (h harness) member(tenant uuid.UUID, perms ...string) uuid.UUID {
	u := uuid.New()
	h.access.Grant(u, tenant, perms...)
	return u
}

// do sends method path as user (uuid.Nil sends none) with a JSON body; a
// write gets a fresh Idempotency-Key.
func (h harness) do(t *testing.T, method, path string, user uuid.UUID, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if user != uuid.Nil {
		req.Header.Set(api.HeaderUserID, user.String())
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// rawGet reads path as user and returns the body as sent.
func (h harness) rawGet(t *testing.T, path string, user uuid.UUID) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderUserID, user.String())
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func want(t *testing.T, status int, body map[string]any, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || (code != "" && body["code"] != code) {
		t.Fatalf("got %d %v, want %d %s", status, body, wantStatus, code)
	}
}

// postForm uploads body through a signed form returned by the API.
func postForm(t *testing.T, upload map[string]any, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range upload["fields"].(map[string]any) {
		_ = mw.WriteField(k, v.(string))
	}
	fw, _ := mw.CreateFormFile("file", "contacts.csv")
	_, _ = fw.Write(body)
	_ = mw.Close()
	resp, err := http.Post(upload["url"].(string), mw.FormDataContentType(), &buf) //nolint:gosec // G107: the test's storage
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("upload answered %d", resp.StatusCode)
	}
}

// finishedImport runs an import with one bad row, so it has an error
// report, and returns its ID.
func (h harness) finishedImport(t *testing.T, tenant, admin uuid.UUID) string {
	t.Helper()
	base := "/v1/contacts/tenants/" + tenant.String()
	status, created := h.do(t, http.MethodPost, base+"/imports", admin, map[string]any{"file_name": "list.csv", "content_type": "text/csv"})
	want(t, status, created, http.StatusCreated, "")
	id := created["import"].(map[string]any)["id"].(string)
	postForm(t, created["upload"].(map[string]any), []byte("phone,first_name\n0700000001,Ann\n0200000002,Ben\n"))
	status, got := h.do(t, http.MethodPost, base+"/imports/"+id+"/upload-confirmation", admin, nil)
	want(t, status, got, http.StatusAccepted, "")
	for range 5 {
		if n, _ := h.svc.ProcessImports(context.Background()); n == 0 {
			break
		}
	}
	return id
}

// TestTenantOperations_NeedContactsPermissions: every
// /v1/contacts/tenants/ operation is 401 without a user, VAL-4003 for
// someone outside the account and AUTH-4024 for a member without the
// contacts permissions; a reader may read (and ask for a download
// link) but not write. It fails if an operation has no case.
func TestTenantOperations_NeedContactsPermissions(t *testing.T) {
	h := newHarness(t)
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/contacts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	admin := h.member(tenant, app.PermRead, app.PermManage)
	base := "/v1/contacts/tenants/" + tenant.String()
	_, c := h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": "0700000009"})
	_, g := h.do(t, http.MethodPost, base+"/groups", admin, map[string]any{"name": "G"})
	h.do(t, http.MethodPost, base+"/attribute-definitions", admin, map[string]any{"key": "county", "label": "County", "type": "text"})
	importID := h.finishedImport(t, tenant, admin)
	filter := map[string]any{"all": []any{map[string]any{"field": "attributes.county", "op": "exists"}}}
	_, seg := h.do(t, http.MethodPost, base+"/segments", admin, map[string]any{"name": "S", "filter": filter})

	member := h.member(tenant)
	reader := h.member(tenant, app.PermRead)
	// POSTs a reader may make: a download link, and a consent lookup (a
	// POST so the number stays out of the URL).
	readerMayWrite := map[string]int{"createImportDownloadLink": http.StatusCreated, "lookupConsent": http.StatusOK}
	bodies := map[string]any{
		"createContact":             map[string]any{"phone": "0700000010"},
		"updateContact":             map[string]any{"first_name": "X"},
		"createAttributeDefinition": map[string]any{"key": "tier", "label": "Tier", "type": "text"},
		"createGroup":               map[string]any{"name": "H"},
		"updateGroup":               map[string]any{"name": "I"},
		"addGroupMembers":           map[string]any{"contact_ids": []string{c["id"].(string)}},
		"createImport":              map[string]any{"file_name": "a.csv", "content_type": "text/csv"},
		"createImportDownloadLink":  map[string]any{"file": "errors"},
		"createSegment":             map[string]any{"name": "T", "filter": filter},
		"updateSegment":             map[string]any{"name": "U"},
		"recordConsent":             map[string]any{"phone": "0700000011", "channel": "sms", "purpose": "promotional", "status": "granted"},
		"lookupConsent":             map[string]any{"phone": "0700000011"},
		"addSuppression":            map[string]any{"phone": "0700000011", "reason": "other"},
		"removeSuppression":         map[string]any{"phone": "0700000011"},
		"issuePreferenceLink":       map[string]any{"phone": "0700000011"},
	}
	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/contacts/tenants/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := strings.NewReplacer("{tenantId}", tenant.String(), "{contactId}", c["id"].(string),
				"{groupId}", g["id"].(string), "{importId}", importID, "{attributeKey}", "county",
				"{segmentId}", seg["id"].(string)).Replace(path)
			body := bodies[op.OperationID]
			t.Run(op.OperationID, func(t *testing.T) {
				status, got := h.do(t, method, concrete, uuid.Nil, body)
				want(t, status, got, http.StatusUnauthorized, "AUTH-4010")
				status, got = h.do(t, method, concrete, uuid.New(), body)
				want(t, status, got, http.StatusNotFound, "VAL-4003")
				status, got = h.do(t, method, concrete, member, body)
				want(t, status, got, http.StatusForbidden, "AUTH-4024")
				status, got = h.do(t, method, concrete, reader, body)
				switch {
				case method == http.MethodGet:
					want(t, status, got, http.StatusOK, "")
				case readerMayWrite[op.OperationID] != 0:
					want(t, status, got, readerMayWrite[op.OperationID], "")
				default:
					want(t, status, got, http.StatusForbidden, "AUTH-4024")
				}
			})
		}
	}
	sort.Strings(ops)
	wantOps := "addGroupMembers,addSuppression,confirmImportUpload,createAttributeDefinition,createContact,createGroup,createImport," +
		"createImportDownloadLink,createSegment,deleteAttributeDefinition,deleteContact,deleteGroup,deleteSegment,getContact," +
		"getGroup,getImport,getSegment,getSegmentCount,issuePreferenceLink,listAttributeDefinitions,listContacts,listGroupMembers," +
		"listGroups,listImportErrors,listImports,listSegmentContacts,listSegments,listSuppressions,lookupConsent,recordConsent," +
		"removeGroupMember,removeSuppression,updateContact,updateGroup,updateSegment"
	if strings.Join(ops, ",") != wantOps {
		t.Fatalf("tenant operations in the spec %v, cases written for %s: add the new one's case", ops, wantOps)
	}
}

func TestContactsAPI_Flow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	admin := h.member(tenant, app.PermRead, app.PermManage)
	base := "/v1/contacts/tenants/" + tenant.String()

	status, got := h.do(t, http.MethodPost, base+"/attribute-definitions", admin, map[string]any{"key": "visits", "label": "Visits", "type": "number"})
	want(t, status, got, http.StatusCreated, "")
	status, created := h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": "0700 000 001", "attributes": map[string]string{"visits": "3"}})
	want(t, status, created, http.StatusCreated, "")
	if created["phone"] != "+254700000001" || created["attributes"].(map[string]any)["visits"] != float64(3) {
		t.Fatalf("created %v: the number in E.164 and visits as a JSON number", created)
	}
	status, merged := h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": "+254700000001", "first_name": "Ann"})
	if status != http.StatusOK || merged["id"] != created["id"] || merged["first_name"] != "Ann" {
		t.Fatalf("merge: %d %v", status, merged)
	}
	status, got = h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": "0200000001"})
	want(t, status, got, http.StatusUnprocessableEntity, "VAL-4020")
	status, got = h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": "0700000002", "attributes": map[string]string{"age": "3"}})
	want(t, status, got, http.StatusUnprocessableEntity, "VAL-4021")
	status, got = h.do(t, http.MethodPatch, base+"/contacts/"+created["id"].(string), admin, map[string]any{"attributes": map[string]any{"visits": nil}})
	if status != http.StatusOK || len(got["attributes"].(map[string]any)) != 0 {
		t.Fatalf("removing an attribute: %d %v", status, got)
	}

	importID := h.finishedImport(t, tenant, admin)
	status, imp := h.do(t, http.MethodGet, base+"/imports/"+importID, admin, nil)
	if status != http.StatusOK || imp["status"] != "completed_with_errors" || imp["rows_valid"] != float64(1) || imp["has_error_report"] != true {
		t.Fatalf("import: %d %v", status, imp)
	}
	status, got = h.do(t, http.MethodPost, base+"/imports/"+importID+"/upload-confirmation", admin, nil)
	want(t, status, got, http.StatusConflict, "VAL-4023")
	status, errs := h.do(t, http.MethodGet, base+"/imports/"+importID+"/errors", admin, nil)
	if status != http.StatusOK || len(errs["errors"].([]any)) != 1 {
		t.Fatalf("errors: %d %v", status, errs)
	}

	status, link := h.do(t, http.MethodPost, base+"/imports/"+importID+"/download-links", admin, map[string]any{"file": "original"})
	want(t, status, link, http.StatusCreated, "")
	path := strings.TrimPrefix(link["url"].(string), "https://api.vaspro.test")
	resp, err := http.Get(h.srv.URL + path) //nolint:gosec // G107: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "phone,first_name\n0700000001,Ann\n0200000002,Ben\n" ||
		resp.Header.Get("Content-Type") != "text/csv" || resp.Header.Get("Content-Disposition") != `attachment; filename=list.csv` ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	status, got = h.do(t, http.MethodGet, "/v1/contacts/downloads/not.valid", uuid.Nil, nil)
	want(t, status, got, http.StatusNotFound, "VAL-4003")

	// Two days on, the files are gone: 410 for a new link and the old one.
	h.clk.Advance(48 * time.Hour)
	if _, err := h.svc.SweepImportFiles(ctx); err != nil {
		t.Fatal(err)
	}
	status, got = h.do(t, http.MethodPost, base+"/imports/"+importID+"/download-links", admin, map[string]any{"file": "errors"})
	want(t, status, got, http.StatusGone, "VAL-4024")
	h.clk.Set(h.clk.Now().Add(-48 * time.Hour)) // the old link's own clock
	status, got = h.do(t, http.MethodGet, path, uuid.Nil, nil)
	want(t, status, got, http.StatusGone, "VAL-4024")
	status, imp = h.do(t, http.MethodGet, base+"/imports/"+importID, admin, nil)
	if status != http.StatusOK || imp["deleted_at"] == nil || imp["rows_valid"] != float64(1) {
		t.Fatalf("after deletion: %d %v", status, imp)
	}
}

// TestSegmentsAPI: a segment saved and evaluated over HTTP, its filter
// returned as sent (a long number's digits kept), and VAL-4025 with the
// reason for a filter that isn't valid or names something deleted.
func TestSegmentsAPI(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	admin := h.member(tenant, app.PermRead, app.PermManage)
	base := "/v1/contacts/tenants/" + tenant.String()
	h.do(t, http.MethodPost, base+"/attribute-definitions", admin, map[string]any{"key": "visits", "label": "Visits", "type": "number"})
	for i, v := range []string{"2", "5", "12345678901234567890.5"} {
		status, got := h.do(t, http.MethodPost, base+"/contacts", admin, map[string]any{"phone": fmt.Sprintf("070000002%d", i), "attributes": map[string]string{"visits": v}})
		want(t, status, got, http.StatusCreated, "")
	}
	filter := json.RawMessage(`{"all":[{"field":"attributes.visits","op":"between","value":[3,12345678901234567890.5]}]}`)
	status, seg := h.do(t, http.MethodPost, base+"/segments", admin, map[string]any{"name": "regulars", "filter": filter})
	want(t, status, seg, http.StatusCreated, "")
	raw, _ := json.Marshal(seg["filter"])
	if !strings.Contains(h.rawGet(t, base+"/segments/"+seg["id"].(string), admin), "12345678901234567890.5") {
		t.Fatalf("the filter lost its digits: %s", raw)
	}
	status, count := h.do(t, http.MethodGet, base+"/segments/"+seg["id"].(string)+"/count", admin, nil)
	if want(t, status, count, http.StatusOK, ""); count["count"] != float64(2) {
		t.Fatalf("count %v", count)
	}
	status, list := h.do(t, http.MethodGet, base+"/segments/"+seg["id"].(string)+"/contacts?limit=1", admin, nil)
	if want(t, status, list, http.StatusOK, ""); len(list["contacts"].([]any)) != 1 || list["next_cursor"] == nil {
		t.Fatalf("page %v", list)
	}

	status, got := h.do(t, http.MethodPost, base+"/segments", admin, map[string]any{"name": "bad",
		"filter": map[string]any{"all": []any{map[string]any{"field": "attributes.visits", "op": "contains", "value": "1"}}}})
	want(t, status, got, http.StatusUnprocessableEntity, "VAL-4025")
	if !strings.Contains(got["detail"].(string), "contains") {
		t.Fatalf("detail %v", got["detail"])
	}
	status, got = h.do(t, http.MethodPost, base+"/segments", admin, map[string]any{"name": "regulars", "filter": filter})
	want(t, status, got, http.StatusBadRequest, "VAL-4000")

	status, got = h.do(t, http.MethodDelete, base+"/attribute-definitions/visits", admin, nil)
	want(t, status, got, http.StatusNoContent, "")
	status, got = h.do(t, http.MethodGet, base+"/segments/"+seg["id"].(string)+"/count", admin, nil)
	want(t, status, got, http.StatusUnprocessableEntity, "VAL-4025")
	status, got = h.do(t, http.MethodDelete, base+"/segments/"+seg["id"].(string), admin, nil)
	want(t, status, got, http.StatusNoContent, "")
}

// TestConsentAPI: consent, suppressions and a preference link over HTTP,
// then the public page and its API with no session: the page's headers,
// no inline script, the masked number only; the person's change is
// VAL-4026 for the client; tampered links answer 404 and VAL-4003.
func TestConsentAPI(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	admin := h.member(tenant, app.PermRead, app.PermManage)
	base := "/v1/contacts/tenants/" + tenant.String()

	status, got := h.do(t, http.MethodPost, base+"/consents", admin, map[string]any{"phone": "0700000031", "channel": "sms", "purpose": "promotional", "status": "granted", "evidence": "signed up at the till"})
	if want(t, status, got, http.StatusOK, ""); got["actor"] != "client" || got["evidence"] != "signed up at the till" {
		t.Fatalf("recorded %v", got)
	}
	status, got = h.do(t, http.MethodPost, base+"/suppressions", admin, map[string]any{"phone": "0700000032", "channel": "whatsapp", "reason": "complaint"})
	want(t, status, got, http.StatusCreated, "")
	status, got = h.do(t, http.MethodPost, base+"/suppressions", admin, map[string]any{"phone": "+254700000032", "channel": "whatsapp", "reason": "other"})
	if want(t, status, got, http.StatusOK, ""); got["reason"] != "complaint" {
		t.Fatalf("the existing entry: %v", got)
	}
	status, got = h.do(t, http.MethodGet, base+"/suppressions?limit=10", admin, nil)
	if want(t, status, got, http.StatusOK, ""); len(got["suppressions"].([]any)) != 1 {
		t.Fatalf("list %v", got)
	}
	status, got = h.do(t, http.MethodPost, base+"/consent-lookups", admin, map[string]any{"phone": "0700000032"})
	if want(t, status, got, http.StatusOK, ""); len(got["decisions"].([]any)) != 10 || len(got["suppressions"].([]any)) != 1 {
		t.Fatalf("lookup %v", got)
	}
	status, got = h.do(t, http.MethodPost, base+"/suppression-removals", admin, map[string]any{"phone": "0700000032", "channel": "whatsapp"})
	want(t, status, got, http.StatusNoContent, "")
	status, got = h.do(t, http.MethodPost, base+"/suppression-removals", admin, map[string]any{"phone": "0700000032", "channel": "whatsapp"})
	want(t, status, got, http.StatusNotFound, "VAL-4003")
	status, got = h.do(t, http.MethodPost, base+"/consents", admin, map[string]any{"phone": "0200000031", "channel": "sms", "purpose": "promotional", "status": "granted"})
	want(t, status, got, http.StatusUnprocessableEntity, "VAL-4020")

	status, link := h.do(t, http.MethodPost, base+"/preference-links", admin, map[string]any{"phone": "0700000031"})
	want(t, status, link, http.StatusCreated, "")
	token := strings.TrimPrefix(link["url"].(string), "https://api.vaspro.test/p/")

	t.Run("the page", func(t *testing.T) {
		resp, err := http.Get(h.srv.URL + "/p/" + token) //nolint:gosec,noctx // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		page := string(body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
			resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %v", resp.StatusCode, resp.Header)
		}
		if !strings.Contains(page, "254 7•• ••• 031") || strings.Contains(page, "700000031") || !strings.Contains(page, `name="sms" checked`) {
			t.Fatalf("page %s", page)
		}
		if strings.Contains(page, "<script>") || strings.Count(page, "<script") != 1 || !strings.Contains(page, `<script src="/p/static/preferences.js">`) {
			t.Fatalf("scripts on the page: %s", page)
		}
		for path, ctype := range map[string]string{"/p/static/preferences.js": "text/javascript", "/p/static/preferences.css": "text/css"} {
			resp, err := http.Get(h.srv.URL + path) //nolint:gosec,noctx // the test's own server
			if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
				t.Fatalf("%s: %v %v", path, resp, err)
			}
			_ = resp.Body.Close()
		}
		bad := token[:11] + "x"
		if bad == token {
			bad = token[:11] + "y"
		}
		resp, err = http.Get(h.srv.URL + "/p/" + bad) //nolint:gosec,noctx // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), `id="choices"`) {
			t.Fatalf("a tampered link: %d %s", resp.StatusCode, body)
		}
		resp, err = http.Get(h.srv.URL + "/p/%3Cscript%3Ealert(1)%3C%2Fscript%3E") //nolint:gosec,noctx // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "<script>alert") {
			t.Fatal("markup from the path was rendered")
		}
	})

	t.Run("its API, with no session", func(t *testing.T) {
		status, got := h.do(t, http.MethodGet, "/v1/contacts/preferences/"+token, uuid.Nil, nil)
		if want(t, status, got, http.StatusOK, ""); got["masked_phone"] != "+254 7•• ••• 031" {
			t.Fatalf("%v", got)
		}
		status, got = h.do(t, http.MethodPut, "/v1/contacts/preferences/"+token, uuid.Nil, map[string]any{"channels": map[string]bool{"sms": false}})
		if want(t, status, got, http.StatusOK, ""); got["channels"].([]any)[0].(map[string]any)["receiving"] != false {
			t.Fatalf("%v", got)
		}
		status, got = h.do(t, http.MethodPost, base+"/consents", admin, map[string]any{"phone": "0700000031", "channel": "sms", "purpose": "promotional", "status": "granted"})
		want(t, status, got, http.StatusConflict, "VAL-4026")
		status, got = h.do(t, http.MethodGet, "/v1/contacts/preferences/"+token[:11]+"!", uuid.Nil, nil)
		want(t, status, got, http.StatusNotFound, "VAL-4003")
	})
}
