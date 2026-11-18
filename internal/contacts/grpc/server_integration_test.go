//go:build integration

package grpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	contactsv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/contacts/v1"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/contactaccess"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	contactsgrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/grpc"
)

// The numbers are the reserved KE test block, +254 700 000 xxx, and
// +254 100 000 xxx; never real ones.

type harness struct {
	client   contactsv1.ContactsServiceClient
	svc      *app.Service
	super    *vasdb.Pool
	accounts *contactaccess.Fake
}

// newHarness serves contacts' gRPC API over an in-memory listener,
// backed by a real app.Service on Postgres as contacts_app.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/contacts"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE contacts_app LOGIN PASSWORD 'contacts_app'; GRANT contacts_service TO contacts_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("contacts_app", "contacts_app")+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	accounts := contactaccess.New()
	svc := app.New(app.Deps{
		Pool: pool, Clock: clock.NewFake(time.Now().UTC()), Accounts: accounts, Packs: map[string]*packs.Pack{"KE": ke},
		DownloadBaseURL: "https://api.vaspro.test",
	})
	lis := bufconn.Listen(1 << 20)
	srv := gogrpc.NewServer()
	contactsgrpc.Register(srv, svc, nil)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := gogrpc.NewClient("passthrough:///bufnet",
		gogrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		gogrpc.WithTransportCredentials(insecure.NewCredentials()),
		gogrpc.WithDefaultCallOptions(gogrpc.MaxCallRecvMsgSize(16<<20), gogrpc.MaxCallSendMsgSize(16<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &harness{client: contactsv1.NewContactsServiceClient(conn), svc: svc, super: super, accounts: accounts}
}

func (h *harness) manager() (uuid.UUID, uuid.UUID) {
	user, tenant := uuid.New(), uuid.New()
	h.accounts.Grant(user, tenant, app.PermRead, app.PermManage)
	return user, tenant
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// TestCheckConsent_AgreesWithTheDomainRule sets up a number for every
// recorded state × suppression, on two channels, then asks CheckConsent
// for each channel and purpose: every answer must equal the decision
// LookupConsent makes for that number one at a time.
func TestCheckConsent_AgreesWithTheDomainRule(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	user, tenant := h.manager()
	other, otherTenant := h.manager()

	type setup struct{ status, actor, suppression string }
	var numbers []string
	n := 0
	for _, status := range []string{"", "granted", "withdrawn"} {
		for _, actor := range []string{"client", "subject"} {
			for _, supp := range []string{"none", "sms", "whatsapp", "every"} {
				if status == "" && actor == "subject" {
					continue
				}
				n++
				number := fmt.Sprintf("0700000%03d", n)
				numbers = append(numbers, number)
				for _, ch := range []string{"sms", "whatsapp"} {
					for _, p := range domain.Purposes {
						if status == "" || (p == domain.PurposeTransactional && ch == "whatsapp") {
							continue
						}
						if actor == "subject" {
							if p != domain.PurposePromotional {
								continue
							}
							word := map[string]string{"granted": "START", "withdrawn": "STOP"}[status]
							if _, err := h.svc.RecordReplyKeyword(ctx, tenant, ch, number, word); err != nil {
								t.Fatal(err)
							}
							continue
						}
						if _, err := h.svc.RecordConsent(ctx, user, tenant, app.ConsentInput{Phone: number, Channel: ch, Purpose: p, Status: status}); err != nil {
							t.Fatal(err)
						}
					}
				}
				switch supp {
				case "sms", "whatsapp":
					if _, _, err := h.svc.AddSuppression(ctx, user, tenant, app.SuppressionInput{Phone: number, Channel: supp, Reason: "other"}); err != nil {
						t.Fatal(err)
					}
				case "every":
					if _, _, err := h.svc.AddSuppression(ctx, user, tenant, app.SuppressionInput{Phone: number, Reason: "complaint"}); err != nil {
						t.Fatal(err)
					}
				}
				// Another account withdraws and suppresses every number: none of it may show.
				if _, err := h.svc.RecordConsent(ctx, other, otherTenant, app.ConsentInput{Phone: number, Channel: "sms", Purpose: "transactional", Status: "withdrawn"}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := h.svc.AddSuppression(ctx, other, otherTenant, app.SuppressionInput{Phone: number, Reason: "other"}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Local and international spellings, a repeat, and two that aren't numbers.
	asked := append(append([]string{}, numbers...), "+254 700 000 001", "254700000002", "0700000001", "0200000001", "hello")
	checked := 0
	for _, ch := range domain.Channels {
		for _, p := range domain.Purposes {
			res, err := h.client.CheckConsent(ctx, &contactsv1.CheckConsentRequest{TenantId: tenant.String(), Channel: ch, Purpose: p, Numbers: asked})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.GetAnswers()) != len(asked) {
				t.Fatalf("%d answers for %d numbers", len(res.GetAnswers()), len(asked))
			}
			for i, a := range res.GetAnswers() {
				if i >= len(asked)-2 {
					if a.GetAllowed() || a.GetReason() != "invalid_number" || a.GetNumber() != asked[i] {
						t.Fatalf("%q: %+v", asked[i], a)
					}
					continue
				}
				st, err := h.svc.LookupConsent(ctx, user, tenant, asked[i])
				if err != nil {
					t.Fatal(err)
				}
				if a.GetNumber() != st.Phone {
					t.Fatalf("%q answered as %q, want %q", asked[i], a.GetNumber(), st.Phone)
				}
				for _, d := range st.Decisions {
					if d.Channel == ch && d.Purpose == p && (d.Allowed != a.GetAllowed() || d.Reason != a.GetReason()) {
						t.Fatalf("%s %s %s: CheckConsent %+v, LookupConsent %+v", asked[i], ch, p, a, d)
					}
				}
				checked++
			}
		}
	}
	reasons := map[string]int{}
	res, _ := h.client.CheckConsent(ctx, &contactsv1.CheckConsentRequest{TenantId: tenant.String(), Channel: "sms", Purpose: "promotional", Numbers: numbers})
	for _, a := range res.GetAnswers() {
		reasons[a.GetReason()]++
	}
	if reasons[""] == 0 || reasons["suppressed"] == 0 || reasons["opted_out"] == 0 {
		t.Fatalf("the setup didn't reach every outcome: %v", reasons)
	}
	t.Logf("%d answers checked against LookupConsent; sms promotional: %v", checked, reasons)
}

func TestCheckConsent_Refusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	_, tenant := h.manager()
	at := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("+2547%08d", i)
		}
		return out
	}
	res, err := h.client.CheckConsent(ctx, &contactsv1.CheckConsentRequest{TenantId: tenant.String(), Channel: "sms", Purpose: "promotional", Numbers: at(10000)})
	if err != nil || len(res.GetAnswers()) != 10000 {
		t.Fatalf("10,000 numbers: %d answers, %v", len(res.GetAnswers()), err)
	}
	if res, err := h.client.CheckConsent(ctx, &contactsv1.CheckConsentRequest{TenantId: tenant.String(), Channel: "sms", Purpose: "promotional"}); err != nil || len(res.GetAnswers()) != 0 {
		t.Fatalf("no numbers: %v, %v", res, err)
	}
	for name, req := range map[string]*contactsv1.CheckConsentRequest{
		"10,001 numbers":            {TenantId: tenant.String(), Channel: "sms", Purpose: "promotional", Numbers: at(10001)},
		"an unknown channel":        {TenantId: tenant.String(), Channel: "fax", Purpose: "promotional", Numbers: at(1)},
		"an unknown purpose":        {TenantId: tenant.String(), Channel: "sms", Purpose: "marketing", Numbers: at(1)},
		"no tenant":                 {Channel: "sms", Purpose: "promotional", Numbers: at(1)},
		"a tenant that isn't an ID": {TenantId: "acme", Channel: "sms", Purpose: "promotional", Numbers: at(1)},
	} {
		_, err := h.client.CheckConsent(ctx, req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExpandAudience(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	user, tenant := h.manager()
	other, otherTenant := h.manager()
	if _, err := h.svc.CreateAttribute(ctx, user, tenant, domain.AttributeDef{Key: "county", Label: "County", Type: "text"}); err != nil {
		t.Fatal(err)
	}
	g, err := h.svc.CreateGroup(ctx, user, tenant, "VIP", "")
	if err != nil {
		t.Fatal(err)
	}
	var members []uuid.UUID
	for i := 1; i <= 7; i++ {
		county := "Nairobi"
		if i > 5 {
			county = "Mombasa"
		}
		c, _, err := h.svc.CreateContact(ctx, user, tenant, app.ContactInput{Phone: fmt.Sprintf("070000080%d", i), Attributes: map[string]string{"county": county}})
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, c.ID)
	}
	if _, err := h.svc.AddGroupMembers(ctx, user, tenant, g.ID, members[:6]); err != nil {
		t.Fatal(err)
	}
	// 801 withdrew SMS promotional by STOP, 802 is suppressed everywhere,
	// 803 the client withdrew; 804 to 807 are allowed.
	if _, err := h.svc.RecordReplyKeyword(ctx, tenant, "sms", "+254700000801", "STOP"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.AddSuppression(ctx, user, tenant, app.SuppressionInput{Phone: "0700000802", Reason: "complaint"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RecordConsent(ctx, user, tenant, app.ConsentInput{Phone: "0700000803", Channel: "sms", Purpose: "promotional", Status: "withdrawn"}); err != nil {
		t.Fatal(err)
	}
	seg, err := h.svc.CreateSegment(ctx, user, tenant, "Nairobi", json.RawMessage(`{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	expand := func(t *testing.T, req *contactsv1.ExpandAudienceRequest) ([]string, map[string]int64, int, error) {
		t.Helper()
		stream, err := h.client.ExpandAudience(ctx, req)
		if err != nil {
			return nil, nil, 0, err
		}
		var got []string
		blocked := map[string]int64{}
		pages := 0
		for {
			page, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, pages, err
			}
			pages++
			for _, r := range page.GetRecipients() {
				if _, err := uuid.Parse(r.GetContactId()); err != nil {
					t.Fatalf("recipient %+v", r)
				}
				got = append(got, strings.TrimPrefix(r.GetNumber(), "+254700000"))
			}
			for k, v := range page.GetBlocked() {
				blocked[k] += v
			}
			if page.GetLast() {
				if _, err := stream.Recv(); err != io.EOF {
					t.Fatalf("a page after the last: %v", err)
				}
				break
			}
		}
		sort.Strings(got)
		return got, blocked, pages, nil
	}

	t.Run("a group, two contacts a page", func(t *testing.T) {
		got, blocked, pages, err := expand(t, &contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: g.ID.String()},
			Channel: "sms", Purpose: "promotional", PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != "[804 805 806]" || blocked["opted_out"] != 2 || blocked["suppressed"] != 1 || len(blocked) != 2 || pages != 3 {
			t.Fatalf("recipients %v, blocked %v, %d pages", got, blocked, pages)
		}
	})
	t.Run("transactional ignores promotional withdrawals", func(t *testing.T) {
		got, blocked, _, err := expand(t, &contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: g.ID.String()},
			Channel: "sms", Purpose: "transactional"})
		if err != nil || fmt.Sprint(got) != "[801 803 804 805 806]" || blocked["suppressed"] != 1 {
			t.Fatalf("%v %v %v", got, blocked, err)
		}
	})
	t.Run("a segment", func(t *testing.T) {
		got, blocked, _, err := expand(t, &contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_SegmentId{SegmentId: seg.ID.String()},
			Channel: "sms", Purpose: "promotional", PageSize: 1})
		if err != nil || fmt.Sprint(got) != "[804 805]" || blocked["opted_out"] != 2 || blocked["suppressed"] != 1 {
			t.Fatalf("%v %v %v", got, blocked, err)
		}
	})
	t.Run("an empty group sends one last page", func(t *testing.T) {
		empty, err := h.svc.CreateGroup(ctx, user, tenant, "empty", "")
		if err != nil {
			t.Fatal(err)
		}
		got, blocked, pages, err := expand(t, &contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: empty.ID.String()},
			Channel: "sms", Purpose: "promotional"})
		if err != nil || len(got) != 0 || len(blocked) != 0 || pages != 1 {
			t.Fatalf("%v %v %d %v", got, blocked, pages, err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		theirs, err := h.svc.CreateGroup(ctx, other, otherTenant, "theirs", "")
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			req  *contactsv1.ExpandAudienceRequest
			code codes.Code
		}{
			"another account's group":          {&contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: theirs.ID.String()}, Channel: "sms", Purpose: "promotional"}, codes.NotFound},
			"its segment from another account": {&contactsv1.ExpandAudienceRequest{TenantId: otherTenant.String(), Audience: &contactsv1.ExpandAudienceRequest_SegmentId{SegmentId: seg.ID.String()}, Channel: "sms", Purpose: "promotional"}, codes.NotFound},
			"no audience":                      {&contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Channel: "sms", Purpose: "promotional"}, codes.InvalidArgument},
			"a bad group ID":                   {&contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: "x"}, Channel: "sms", Purpose: "promotional"}, codes.InvalidArgument},
			"a bad channel":                    {&contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: g.ID.String()}, Channel: "fax", Purpose: "promotional"}, codes.InvalidArgument},
			"pages too large":                  {&contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_GroupId{GroupId: g.ID.String()}, Channel: "sms", Purpose: "promotional", PageSize: 5001}, codes.InvalidArgument},
		} {
			_, _, _, err := expand(t, c.req)
			if status.Code(err) != c.code {
				t.Errorf("%s: %v, want %s", name, err, c.code)
			}
		}
	})
	t.Run("a segment whose attribute was deleted", func(t *testing.T) {
		if err := h.svc.DeleteAttribute(ctx, user, tenant, "county"); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := expand(t, &contactsv1.ExpandAudienceRequest{TenantId: tenant.String(), Audience: &contactsv1.ExpandAudienceRequest_SegmentId{SegmentId: seg.ID.String()}, Channel: "sms", Purpose: "promotional"})
		wantCode(t, err, codes.FailedPrecondition)
	})
}

func TestRecordReplyKeywordAndPreferenceLinks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	user, tenant := h.manager()
	res, err := h.client.RecordReplyKeyword(ctx, &contactsv1.RecordReplyKeywordRequest{TenantId: tenant.String(), Channel: "sms", Number: "+254700000901", Text: "Stop!"})
	if err != nil || res.GetMatched() != "stop" {
		t.Fatalf("%v, %v", res, err)
	}
	if res, err := h.client.RecordReplyKeyword(ctx, &contactsv1.RecordReplyKeywordRequest{TenantId: tenant.String(), Channel: "sms", Number: "+254700000901", Text: "thanks"}); err != nil || res.GetMatched() != "" {
		t.Fatalf("%v, %v", res, err)
	}
	st, err := h.svc.LookupConsent(ctx, user, tenant, "0700000901")
	if err != nil || len(st.Consents) != 1 || st.Consents[0].Actor != "subject" {
		t.Fatalf("%+v, %v", st, err)
	}
	_, err = h.client.RecordReplyKeyword(ctx, &contactsv1.RecordReplyKeywordRequest{TenantId: tenant.String(), Channel: "sms", Number: "12", Text: "STOP"})
	wantCode(t, err, codes.InvalidArgument)

	link, err := h.client.IssuePreferenceLink(ctx, &contactsv1.IssuePreferenceLinkRequest{TenantId: tenant.String(), Number: "+254700000901"})
	if err != nil || !strings.HasPrefix(link.GetUrl(), "https://api.vaspro.test/p/") || link.GetExpiresAtUnix() <= time.Now().Unix() {
		t.Fatalf("%v, %v", link, err)
	}
	p, err := h.svc.GetPreferences(ctx, strings.TrimPrefix(link.GetUrl(), "https://api.vaspro.test/p/"))
	if err != nil || p.Channels[0].Receiving {
		t.Fatalf("the page after STOP: %+v, %v", p, err)
	}
	_, err = h.client.IssuePreferenceLink(ctx, &contactsv1.IssuePreferenceLinkRequest{TenantId: "x", Number: "+254700000901"})
	wantCode(t, err, codes.InvalidArgument)
}

// TestCheckConsent_Latency is the phase's exit-gate check: 10,000
// numbers answered with p95 under 200ms (data model §10, OQ-26), in an
// account with 200,000 recorded consents and 20,000 suppressions.
func TestCheckConsent_Latency(t *testing.T) {
	if testing.Short() {
		t.Skip("200,000 consents")
	}
	ctx := context.Background()
	h := newHarness(t)
	_, tenant := h.manager()
	// Numbers under country code 999, which no country has.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO consents (tenant_id, phone_e164, channel, purpose, status, actor, source, recorded_by, recorded_at)
		SELECT $1, '+999' || lpad(i::text, 9, '0'), 'sms', p,
		       CASE WHEN i % 3 = 0 THEN 'withdrawn' ELSE 'granted' END, 'client', 'api', gen_random_uuid(), now()
		FROM generate_series(1, 100000) i, unnest(ARRAY['transactional', 'promotional']::contacts.purpose[]) p`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO suppressions (tenant_id, phone_e164, channel, reason, created_by)
		SELECT $1, '+999' || lpad((i * 5)::text, 9, '0'), CASE WHEN i % 2 = 0 THEN NULL ELSE 'sms'::contacts.channel END, 'other', gen_random_uuid()
		FROM generate_series(1, 20000) i`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `ANALYZE consents; ANALYZE suppressions`); err != nil {
		t.Fatal(err)
	}
	numbers := make([]string, 10000)
	for i := range numbers {
		numbers[i] = fmt.Sprintf("+999%09d", i*7+1) // spread over the consents
	}
	req := &contactsv1.CheckConsentRequest{TenantId: tenant.String(), Channel: "sms", Purpose: "promotional", Numbers: numbers}
	for range 3 { // warm up
		if _, err := h.client.CheckConsent(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var took []time.Duration
	var res *contactsv1.CheckConsentResponse
	for range 50 {
		start := time.Now()
		var err error
		res, err = h.client.CheckConsent(ctx, req)
		took = append(took, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p50, p95 := took[24], took[47]
	reasons := map[string]int{}
	for _, a := range res.GetAnswers() {
		reasons[a.GetReason()]++
	}
	t.Logf("10,000 numbers over gRPC, 50 calls: p50 %v, p95 %v, max %v; answers %v", p50, p95, took[49], reasons)
	if reasons[""] == 0 || reasons["opted_out"] == 0 || reasons["suppressed"] == 0 {
		t.Fatalf("the data didn't reach every outcome: %v", reasons)
	}
	if p95 > 200*time.Millisecond {
		t.Fatalf("p95 %v, over 200ms", p95)
	}
}
