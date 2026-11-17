//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// segmentFixture is one account with four attributes, two groups and
// nine contacts (+254700000101 to 109), and a second account whose
// contacts match every filter, so a leak shows.
type segmentFixture struct {
	h              *harness
	m, other       member
	groupA, groupB uuid.UUID
	ids            map[uuid.UUID]string // contact ID → its last three digits
}

func newSegmentFixture(t *testing.T) *segmentFixture {
	t.Helper()
	ctx := context.Background()
	h := newHarness(t)
	f := &segmentFixture{h: h, m: h.manager(), other: h.manager(), ids: map[uuid.UUID]string{}}
	for _, m := range []member{f.m, f.other} {
		for _, d := range []domain.AttributeDef{
			{Key: "county", Label: "County", Type: "text"}, {Key: "age", Label: "Age", Type: "number"},
			{Key: "joined", Label: "Joined", Type: "date"}, {Key: "vip", Label: "VIP", Type: "boolean"},
		} {
			if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	a, err := h.svc.CreateGroup(ctx, f.m.user, f.m.tenant, "A", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.svc.CreateGroup(ctx, f.m.user, f.m.tenant, "B", "")
	if err != nil {
		t.Fatal(err)
	}
	f.groupA, f.groupB = a.ID, b.ID

	type row struct {
		last, first, email, county, age, joined, vip string
		groups                                       []uuid.UUID
		created                                      string // RFC 3339, UTC
	}
	rows := map[string]row{
		"101": {first: "Ann", last: "Wanjiru", email: "Ann@Example.com", county: "Nairobi", age: "34", joined: "2026-01-15", vip: "true", groups: []uuid.UUID{a.ID}, created: "2026-01-10T09:00:00Z"},
		"102": {first: "Ben", last: "Otieno", county: "Mombasa", age: "17", joined: "2025-12-31", vip: "false", groups: []uuid.UUID{b.ID}, created: "2026-01-31T22:30:00Z"},
		"103": {first: "Cy", last: "Wambui", county: "Nairobi", age: "65", joined: "2026-03-01", groups: []uuid.UUID{a.ID, b.ID}, created: "2026-01-31T20:59:59Z"},
		"104": {first: "Di", last: "Kamau", county: "Kisumu", age: "18.5", created: "2026-02-15T12:00:00Z"},
		"105": {last: "Njeri", county: "nairobi", age: "-3.50", vip: "true", created: "2026-03-01T00:00:00Z"},
		"106": {first: "Eve", created: "2026-03-02T00:00:00Z"},
		"107": {first: "Fay", last: "a.b*c", county: "Nakuru", age: "100", created: "2026-03-03T00:00:00Z"},
		"108": {first: "Gus", last: `Back\slash "Q"`, county: "Nairobi", age: "30", joined: "2026-01-01", created: "2026-03-04T00:00:00Z"},
		"109": {first: "Hal", last: "(a|b)", email: "hal@example.org", created: "2026-03-05T00:00:00Z"},
	}
	for digits, r := range rows {
		attrs := map[string]string{"county": r.county, "age": r.age, "joined": r.joined, "vip": r.vip}
		for k, v := range attrs {
			if v == "" {
				delete(attrs, k)
			}
		}
		for _, m := range []member{f.m, f.other} {
			c, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{
				Phone: "0700000" + digits, Fields: domain.ContactFields{FirstName: r.first, LastName: r.last, Email: r.email}, Attributes: attrs,
			})
			if err != nil {
				t.Fatalf("%s: %v", digits, err)
			}
			if _, err := h.super.Exec(ctx, `UPDATE contacts SET created_at = $1 WHERE id = $2`, r.created, c.ID); err != nil {
				t.Fatal(err)
			}
			if m == f.m {
				f.ids[c.ID] = digits
				for _, g := range r.groups {
					if _, err := h.svc.AddGroupMembers(ctx, m.user, m.tenant, g, []uuid.UUID{c.ID}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	return f
}

// members lists a segment's contacts page by page and returns their
// last three digits, sorted; each must appear once.
func (f *segmentFixture) members(t *testing.T, m member, id uuid.UUID, limit int) []string {
	t.Helper()
	var out []string
	seen := map[uuid.UUID]bool{}
	cursor := ""
	for {
		page, next, err := f.h.svc.ListSegmentContacts(context.Background(), m.user, m.tenant, id, app.Page{Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			if seen[c.ID] {
				t.Fatalf("%s listed twice", c.ID)
			}
			seen[c.ID] = true
			digits, ok := f.ids[c.ID]
			if !ok {
				t.Fatalf("a contact from outside the account: %s", c.Phone)
			}
			out = append(out, digits)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Strings(out)
	return out
}

func TestSegments_Filters(t *testing.T) {
	ctx := context.Background()
	f := newSegmentFixture(t)
	cond := func(field, op, value string) string {
		if value == "" {
			return fmt.Sprintf(`{"field":%q,"op":%q}`, field, op)
		}
		return fmt.Sprintf(`{"field":%q,"op":%q,"value":%s}`, field, op, value)
	}
	all := func(conds ...string) string { return `{"all":[` + strings.Join(conds, ",") + `]}` }
	ga, gb := `"`+f.groupA.String()+`"`, `"`+f.groupB.String()+`"`
	for i, c := range []struct {
		name, filter string
		want         string // last three digits, sorted
	}{
		{"text eq is exact", all(cond("attributes.county", "eq", `"Nairobi"`)), "101 103 108"},
		{"text neq needs a value", all(cond("attributes.county", "neq", `"Nairobi"`)), "102 104 105 107"},
		{"text in", all(cond("attributes.county", "in", `["Mombasa","Kisumu","Nowhere"]`)), "102 104"},
		{"contains", all(cond("last_name", "contains", `"am"`)), "103 104"},
		{"contains a dot is a dot", all(cond("last_name", "contains", `"a.b*"`)), "107"},
		{"contains a quote and a backslash", all(cond("last_name", "contains", `"h \"Q"`)), "108"},
		{"contains brackets and a bar", all(cond("last_name", "contains", `"(a|b)"`)), "109"},
		{"starts_with", all(cond("last_name", "starts_with", `"Wa"`)), "101 103"},
		{"exists", all(cond("attributes.joined", "exists", "")), "101 102 103 108"},
		{"a field never set doesn't exist", all(cond("first_name", "exists", "")), "101 102 103 104 106 107 108 109"},
		{"email ignores case", all(cond("email", "eq", `"ann@example.COM"`)), "101"},
		{"number gt", all(cond("attributes.age", "gt", `30`)), "101 103 107"},
		{"number gte a decimal", all(cond("attributes.age", "gte", `18.5`)), "101 103 104 107 108"},
		{"number lt a negative", all(cond("attributes.age", "lt", `"0"`)), "105"},
		{"number between is inclusive", all(cond("attributes.age", "between", `[18,34]`)), "101 104 108"},
		{"number neq", all(cond("attributes.age", "neq", `100`)), "101 102 103 104 105 108"},
		{"date after", all(cond("attributes.joined", "after", `"2026-01-01"`)), "101 103"},
		{"date eq", all(cond("attributes.joined", "eq", `"2026-01-01"`)), "108"},
		{"date between", all(cond("attributes.joined", "between", `["2025-12-31","2026-01-15"]`)), "101 102 108"},
		{"created_at is a Nairobi date", all(cond("created_at", "eq", `"2026-02-01"`)), "102"},
		{"created_at 23:59:59 in Nairobi is the day before", all(cond("created_at", "eq", `"2026-01-31"`)), "103"},
		{"created_at before", all(cond("created_at", "before", `"2026-02-01"`)), "101 103"},
		{"boolean true", all(cond("attributes.vip", "eq", `true`)), "101 105"},
		{"boolean false", all(cond("attributes.vip", "eq", `false`)), "102"},
		{"group in", all(cond("group", "in", ga)), ""},
		{"group in A", all(cond("group", "in", "["+ga+"]")), "101 103"},
		{"group in A or B", all(cond("group", "in", "["+ga+","+gb+"]")), "101 102 103"},
		{"group not_in A, so no group too", all(cond("group", "not_in", "["+ga+"]")), "102 104 105 106 107 108 109"},
		{"all", all(cond("attributes.county", "eq", `"Nairobi"`), cond("attributes.age", "lt", `40`)), "101 108"},
		{"the data model's example", all(cond("attributes.county", "eq", `"Nairobi"`),
			`{"any":[`+cond("group", "in", "["+gb+"]")+","+cond("attributes.joined", "after", `"2026-01-01"`)+`]}`), "101 103"},
		{"any at the top", `{"any":[` + cond("attributes.vip", "eq", `true`) + "," + cond("last_name", "eq", `"Kamau"`) + `]}`, "101 104 105"},
		{"nothing matches", all(cond("first_name", "eq", `"Zed"`)), "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, fmt.Sprintf("segment %d", i), json.RawMessage(c.filter))
			if c.want == "" {
				if !errors.Is(err, app.ErrFilter) {
					t.Fatalf("CreateSegment: %v, want ErrFilter", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSegment: %v", err)
			}
			want := strings.Fields(c.want)
			if c.want == "none" {
				want = nil
			}
			n, err := f.h.svc.CountSegment(ctx, f.m.user, f.m.tenant, s.ID)
			if err != nil || n != int64(len(want)) {
				t.Fatalf("CountSegment = %d, %v; want %d", n, err, len(want))
			}
			if got := f.members(t, f.m, s.ID, 2); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("members %v, want %v", got, want)
			}
		})
	}
}

func TestSegments_SavedAndChanged(t *testing.T) {
	ctx := context.Background()
	f := newSegmentFixture(t)
	nairobi := json.RawMessage(`{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"}]}`)
	s, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, "Nairobi", nairobi)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Nairobi" || !strings.Contains(string(s.Filter), "Nairobi") {
		t.Fatalf("segment %+v", s)
	}

	t.Run("names are unique ignoring case", func(t *testing.T) {
		if _, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, "NAIROBI", nairobi); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("a second NAIROBI: %v", err)
		}
		if _, err := f.h.svc.CreateSegment(ctx, f.other.user, f.other.tenant, "Nairobi", nairobi); err != nil {
			t.Fatalf("another account's Nairobi: %v", err)
		}
	})

	t.Run("refused filters save nothing", func(t *testing.T) {
		for name, filter := range map[string]string{
			"not valid":               `{"all":[{"field":"phone","op":"eq","value":"1"}]}`,
			"an undefined attribute":  `{"all":[{"field":"attributes.town","op":"eq","value":"x"}]}`,
			"an unknown group":        `{"all":[{"field":"group","op":"in","value":["` + uuid.NewString() + `"]}]}`,
			"another account's group": `{"all":[{"field":"group","op":"in","value":["` + f.otherGroup(t).String() + `"]}]}`,
		} {
			if _, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, name, json.RawMessage(filter)); !errors.Is(err, app.ErrFilter) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, bad := range map[string]string{"no name": "", "a long name": strings.Repeat("n", 101)} {
			if _, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, bad, nairobi); !errors.Is(err, app.ErrValidation) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if n := f.h.count(t, `SELECT count(*) FROM segments WHERE tenant_id = $1`, f.m.tenant); n != 1 {
			t.Fatalf("%d segments, want 1", n)
		}
	})

	t.Run("read and list", func(t *testing.T) {
		got, err := f.h.svc.GetSegment(ctx, f.m.user, f.m.tenant, s.ID)
		if err != nil || got.ID != s.ID {
			t.Fatalf("GetSegment: %+v, %v", got, err)
		}
		if _, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, "adults", json.RawMessage(`{"all":[{"field":"attributes.age","op":"gte","value":18}]}`)); err != nil {
			t.Fatal(err)
		}
		list, err := f.h.svc.ListSegments(ctx, f.m.user, f.m.tenant)
		if err != nil || len(list) != 2 || list[0].Name != "adults" || list[1].Name != "Nairobi" {
			t.Fatalf("ListSegments: %+v, %v", list, err)
		}
	})

	t.Run("a change is checked, then counts at once", func(t *testing.T) {
		if _, err := f.h.svc.UpdateSegment(ctx, f.m.user, f.m.tenant, s.ID, nil, json.RawMessage(`{"all":[]}`)); !errors.Is(err, app.ErrFilter) {
			t.Fatalf("an empty group: %v", err)
		}
		name := "Nairobi adults"
		got, err := f.h.svc.UpdateSegment(ctx, f.m.user, f.m.tenant, s.ID, &name,
			json.RawMessage(`{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"},{"field":"attributes.age","op":"gte","value":18}]}`))
		if err != nil || got.Name != name {
			t.Fatalf("UpdateSegment: %+v, %v", got, err)
		}
		if n, err := f.h.svc.CountSegment(ctx, f.m.user, f.m.tenant, s.ID); err != nil || n != 3 {
			t.Fatalf("count %d, %v; want 3", n, err)
		}
		if _, err := f.h.svc.UpdateSegment(ctx, f.m.user, f.m.tenant, uuid.New(), &name, nil); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("an unknown segment: %v", err)
		}
	})

	t.Run("a new contact is counted at once", func(t *testing.T) {
		if _, _, err := f.h.svc.CreateContact(ctx, f.m.user, f.m.tenant, app.ContactInput{
			Phone: "0700000110", Attributes: map[string]string{"county": "Nairobi", "age": "40"},
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := f.h.svc.CountSegment(ctx, f.m.user, f.m.tenant, s.ID); err != nil || n != 4 {
			t.Fatalf("count %d, %v; want 4", n, err)
		}
	})

	t.Run("an attribute deleted since is refused when used", func(t *testing.T) {
		if err := f.h.svc.DeleteAttribute(ctx, f.m.user, f.m.tenant, "age"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.svc.CountSegment(ctx, f.m.user, f.m.tenant, s.ID); !errors.Is(err, app.ErrFilter) {
			t.Fatalf("count: %v", err)
		}
		if _, _, err := f.h.svc.ListSegmentContacts(ctx, f.m.user, f.m.tenant, s.ID, app.Page{}); !errors.Is(err, app.ErrFilter) {
			t.Fatalf("list: %v", err)
		}
	})

	t.Run("a group deleted since is refused when used", func(t *testing.T) {
		g, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, "in B", json.RawMessage(`{"all":[{"field":"group","op":"in","value":["`+f.groupB.String()+`"]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.h.svc.DeleteGroup(ctx, f.m.user, f.m.tenant, f.groupB); err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.svc.CountSegment(ctx, f.m.user, f.m.tenant, g.ID); !errors.Is(err, app.ErrFilter) {
			t.Fatalf("count: %v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := f.h.svc.DeleteSegment(ctx, f.m.user, f.m.tenant, s.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.h.svc.GetSegment(ctx, f.m.user, f.m.tenant, s.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("read after delete: %v", err)
		}
		if err := f.h.svc.DeleteSegment(ctx, f.m.user, f.m.tenant, s.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("a second delete: %v", err)
		}
	})
}

func (f *segmentFixture) otherGroup(t *testing.T) uuid.UUID {
	t.Helper()
	g, err := f.h.svc.CreateGroup(context.Background(), f.other.user, f.other.tenant, "theirs "+uuid.NewString()[:8], "")
	if err != nil {
		t.Fatal(err)
	}
	return g.ID
}

func TestSegments_Who(t *testing.T) {
	ctx := context.Background()
	f := newSegmentFixture(t)
	filter := json.RawMessage(`{"all":[{"field":"first_name","op":"exists"}]}`)
	s, err := f.h.svc.CreateSegment(ctx, f.m.user, f.m.tenant, "named", filter)
	if err != nil {
		t.Fatal(err)
	}
	reader := member{user: uuid.New(), tenant: f.m.tenant}
	f.h.accounts.Grant(reader.user, reader.tenant, app.PermRead)

	t.Run("a reader reads, counts and lists, but changes nothing", func(t *testing.T) {
		if _, err := f.h.svc.GetSegment(ctx, reader.user, reader.tenant, s.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := f.h.svc.CountSegment(ctx, reader.user, reader.tenant, s.ID); err != nil || n != 8 {
			t.Fatalf("count %d, %v", n, err)
		}
		if got := f.members(t, reader, s.ID, 100); len(got) != 8 {
			t.Fatalf("members %v", got)
		}
		name := "x"
		for what, err := range map[string]error{
			"create": func() error {
				_, err := f.h.svc.CreateSegment(ctx, reader.user, reader.tenant, "x", filter)
				return err
			}(),
			"update": func() error {
				_, err := f.h.svc.UpdateSegment(ctx, reader.user, reader.tenant, s.ID, &name, nil)
				return err
			}(),
			"delete": f.h.svc.DeleteSegment(ctx, reader.user, reader.tenant, s.ID),
		} {
			if !errors.Is(err, app.ErrPermissionDenied) {
				t.Errorf("%s: %v", what, err)
			}
		}
	})

	t.Run("another account finds nothing", func(t *testing.T) {
		if _, err := f.h.svc.GetSegment(ctx, f.other.user, f.other.tenant, s.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("read: %v", err)
		}
		if _, err := f.h.svc.CountSegment(ctx, f.other.user, f.other.tenant, s.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("count: %v", err)
		}
		if _, _, err := f.h.svc.ListSegmentContacts(ctx, f.other.user, f.other.tenant, s.ID, app.Page{}); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("list: %v", err)
		}
		stranger := uuid.New()
		if _, err := f.h.svc.CountSegment(ctx, stranger, f.m.tenant, s.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("a stranger: %v", err)
		}
	})

	t.Run("bad pages", func(t *testing.T) {
		for _, p := range []app.Page{{Limit: 101}, {Limit: -1}, {Cursor: "!!"}} {
			if _, _, err := f.h.svc.ListSegmentContacts(ctx, f.m.user, f.m.tenant, s.ID, p); !errors.Is(err, app.ErrValidation) {
				t.Errorf("%+v: %v", p, err)
			}
		}
	})
}

// TestSegments_Million counts and pages a segment over 1,000,000
// contacts: one sequential pass per evaluation (data model §10's open
// point, measured here).
func TestSegments_Million(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000,000 contacts")
	}
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	for _, d := range []domain.AttributeDef{{Key: "county", Label: "County", Type: "text"}, {Key: "age", Label: "Age", Type: "number"}} {
		if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, d); err != nil {
			t.Fatal(err)
		}
	}
	g, err := h.svc.CreateGroup(ctx, m.user, m.tenant, "tenth", "")
	if err != nil {
		t.Fatal(err)
	}
	// Numbers under country code 999, which no country has.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO contacts (id, tenant_id, phone_e164, first_name, attributes, source, created_at)
		SELECT gen_random_uuid(), $1, '+999' || lpad(i::text, 9, '0'), 'n' || i,
		       jsonb_build_object('county', (ARRAY['Nairobi','Mombasa','Kisumu','Nakuru','Eldoret'])[1 + i % 5], 'age', i % 90),
		       'import', now() - make_interval(secs => i)
		FROM generate_series(1, 1000000) i`, m.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO group_members (group_id, contact_id, tenant_id)
		SELECT $2, id, tenant_id FROM contacts WHERE tenant_id = $1 AND first_name LIKE '%0'`, m.tenant, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `ANALYZE contacts; ANALYZE group_members`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, filter string
		want         int64
	}{
		// i % 5 = 0 → Nairobi (200,000); of those, i % 90 >= 50 is 8 of every 18 (88,888).
		{"attributes", `{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"},{"field":"attributes.age","op":"gte","value":50}]}`, 88888},
		{"a group", `{"all":[{"field":"group","op":"in","value":["` + g.ID.String() + `"]}]}`, 100000},
	} {
		s, err := h.svc.CreateSegment(ctx, m.user, m.tenant, c.name, json.RawMessage(c.filter))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		n, err := h.svc.CountSegment(ctx, m.user, m.tenant, s.ID)
		took := time.Since(start)
		if err != nil || n != c.want {
			t.Fatalf("%s: count %d, %v; want %d", c.name, n, err, c.want)
		}
		start = time.Now()
		page, _, err := h.svc.ListSegmentContacts(ctx, m.user, m.tenant, s.ID, app.Page{Limit: 100})
		pageTook := time.Since(start)
		if err != nil || len(page) != 100 {
			t.Fatalf("%s: a page of %d, %v", c.name, len(page), err)
		}
		t.Logf("%s: counted %d of 1,000,000 in %v; first page of 100 in %v", c.name, n, took, pageTook)
		if took > 10*time.Second {
			t.Fatalf("%s: counting took %v, over 10s", c.name, took)
		}
	}
}
