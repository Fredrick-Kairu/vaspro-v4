//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// decision finds one channel and purpose's decision in a lookup.
func decision(t *testing.T, s app.ConsentState, channel, purpose string) app.ConsentDecision {
	t.Helper()
	for _, d := range s.Decisions {
		if d.Channel == channel && d.Purpose == purpose {
			return d
		}
	}
	t.Fatalf("no decision for %s %s in %+v", channel, purpose, s.Decisions)
	return app.ConsentDecision{}
}

func TestConsent_RecordedByTheClient(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	contact, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: "0700000201"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nothing recorded: KE allows both purposes", func(t *testing.T) {
		s, err := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700 000 201")
		if err != nil {
			t.Fatal(err)
		}
		if s.Phone != "+254700000201" || s.ContactID == nil || *s.ContactID != contact.ID || len(s.Consents) != 0 || len(s.Decisions) != 10 {
			t.Fatalf("state %+v", s)
		}
		for _, d := range s.Decisions {
			if !d.Allowed {
				t.Fatalf("%+v blocked", d)
			}
		}
	})

	t.Run("a withdrawal blocks and is published once", func(t *testing.T) {
		c, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "+254700000201", Channel: "sms", Purpose: "promotional", Status: "withdrawn", Evidence: "asked by phone"})
		if err != nil || c.Status != "withdrawn" || c.Actor != "client" || c.Source != "api" {
			t.Fatalf("RecordConsent: %+v, %v", c, err)
		}
		if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000201", Channel: "sms", Purpose: "promotional", Status: "withdrawn"}); err != nil {
			t.Fatal(err)
		}
		s, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000201")
		if d := decision(t, s, "sms", "promotional"); d.Allowed || d.Reason != "opted_out" {
			t.Fatalf("sms promotional %+v", d)
		}
		if d := decision(t, s, "whatsapp", "promotional"); !d.Allowed {
			t.Fatalf("whatsapp promotional %+v", d)
		}
		evs := h.events(t, "consent.withdrawn")
		if len(evs) != 1 || evs[0]["contact_id"] != contact.ID.String() || evs[0]["actor"] != "client" || evs[0]["source"] != "api" || evs[0]["channel"] != "sms" {
			t.Fatalf("events %v", evs)
		}
		if len(s.History) != 2 || s.History[0].Status != "withdrawn" {
			t.Fatalf("history %+v", s.History)
		}
	})

	t.Run("the client can grant again", func(t *testing.T) {
		c, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000201", Channel: "sms", Purpose: "promotional", Status: "granted", Evidence: "signed up at the till"})
		if err != nil || c.Status != "granted" || c.Evidence != "signed up at the till" {
			t.Fatalf("%+v, %v", c, err)
		}
	})

	t.Run("a number that isn't a contact has consent too, with no contact_id", func(t *testing.T) {
		if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000202", Channel: "whatsapp", Purpose: "transactional", Status: "withdrawn"}); err != nil {
			t.Fatal(err)
		}
		evs := h.events(t, "consent.withdrawn")
		if len(evs) != 2 || evs[1]["contact_id"] != nil {
			t.Fatalf("events %v", evs)
		}
	})

	t.Run("deleting the contact keeps the consent", func(t *testing.T) {
		if err := h.svc.DeleteContact(ctx, m.user, m.tenant, contact.ID); err != nil {
			t.Fatal(err)
		}
		s, err := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000201")
		if err != nil || s.ContactID != nil || len(s.Consents) != 1 {
			t.Fatalf("%+v, %v", s, err)
		}
	})

	t.Run("refusals write nothing", func(t *testing.T) {
		before := h.count(t, `SELECT count(*) FROM consent_events`)
		for name, c := range map[string]struct {
			in   app.ConsentInput
			want error
		}{
			"a landline":         {app.ConsentInput{Phone: "0200000201", Channel: "sms", Purpose: "promotional", Status: "granted"}, app.ErrPhone},
			"an unknown channel": {app.ConsentInput{Phone: "0700000201", Channel: "fax", Purpose: "promotional", Status: "granted"}, app.ErrValidation},
			"an unknown purpose": {app.ConsentInput{Phone: "0700000201", Channel: "sms", Purpose: "marketing", Status: "granted"}, app.ErrValidation},
			"an unknown status":  {app.ConsentInput{Phone: "0700000201", Channel: "sms", Purpose: "promotional", Status: "maybe"}, app.ErrValidation},
			"long evidence":      {app.ConsentInput{Phone: "0700000201", Channel: "sms", Purpose: "promotional", Status: "granted", Evidence: strings.Repeat("e", 501)}, app.ErrValidation},
		} {
			if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, c.in); !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", name, err, c.want)
			}
		}
		if _, err := h.svc.LookupConsent(ctx, m.user, m.tenant, "07000"); !errors.Is(err, app.ErrPhone) {
			t.Errorf("looking up a short number: %v", err)
		}
		if n := h.count(t, `SELECT count(*) FROM consent_events`); n != before {
			t.Fatalf("%d history rows, want %d", n, before)
		}
	})
}

// The person's own withdrawal stands until the person changes it (data
// model §10): STOP is enforced, and only START gives it back.
func TestConsent_ReplyKeywords(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()

	for _, text := range []string{"stopped", "please stop", "STOP!!", "hello"} {
		got, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "sms", "+254700000301", text)
		if err != nil || got != "" {
			t.Fatalf("%q: %q, %v", text, got, err)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM consents`); n != 0 {
		t.Fatalf("no keyword wrote %d consents", n)
	}

	got, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "sms", "+254700000301", " Stop. ")
	if err != nil || got != "stop" {
		t.Fatalf("STOP: %q, %v", got, err)
	}
	s, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000301")
	if d := decision(t, s, "sms", "promotional"); d.Reason != "opted_out" {
		t.Fatalf("after STOP: %+v", d)
	}
	if d := decision(t, s, "sms", "transactional"); !d.Allowed {
		t.Fatalf("STOP stopped transactional: %+v", d)
	}
	if d := decision(t, s, "whatsapp", "promotional"); !d.Allowed {
		t.Fatalf("STOP on sms stopped whatsapp: %+v", d)
	}
	if len(s.Consents) != 1 || s.Consents[0].Actor != "subject" || s.Consents[0].Source != "keyword" {
		t.Fatalf("consents %+v", s.Consents)
	}
	evs := h.events(t, "consent.withdrawn")
	if len(evs) != 1 || evs[0]["actor"] != "subject" || evs[0]["source"] != "keyword" {
		t.Fatalf("events %v", evs)
	}

	t.Run("the client can't undo it", func(t *testing.T) {
		before := h.count(t, `SELECT count(*) FROM consent_events`)
		_, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000301", Channel: "sms", Purpose: "promotional", Status: "granted", Evidence: "they said yes"})
		if !errors.Is(err, app.ErrSubjectWithdrew) {
			t.Fatalf("granting over it: %v", err)
		}
		c, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000301", Channel: "sms", Purpose: "promotional", Status: "withdrawn"})
		if err != nil || c.Actor != "subject" {
			t.Fatalf("withdrawing again: %+v, %v", c, err)
		}
		if n := h.count(t, `SELECT count(*) FROM consent_events`); n != before {
			t.Fatalf("history grew to %d from %d", n, before)
		}
		if len(h.events(t, "consent.withdrawn")) != 1 {
			t.Fatal("a second event")
		}
	})

	t.Run("STOP twice publishes once", func(t *testing.T) {
		if got, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "sms", "+254700000301", "stop"); err != nil || got != "stop" {
			t.Fatalf("%q, %v", got, err)
		}
		if len(h.events(t, "consent.withdrawn")) != 1 {
			t.Fatal("a second event")
		}
	})

	t.Run("START gives it back, and then the client may change it", func(t *testing.T) {
		if got, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "sms", "+254700000301", "start"); err != nil || got != "start" {
			t.Fatalf("%q, %v", got, err)
		}
		s, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000301")
		if d := decision(t, s, "sms", "promotional"); !d.Allowed || s.Consents[0].Status != "granted" || s.Consents[0].Actor != "subject" {
			t.Fatalf("after START: %+v %+v", d, s.Consents)
		}
		if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000301", Channel: "sms", Purpose: "promotional", Status: "withdrawn"}); err != nil {
			t.Fatalf("the client withdrawing: %v", err)
		}
		if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000301", Channel: "sms", Purpose: "promotional", Status: "granted"}); err != nil {
			t.Fatalf("the client's own withdrawal is the client's to undo: %v", err)
		}
	})

	t.Run("a keyword for a number that isn't valid is refused", func(t *testing.T) {
		if _, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "sms", "+25412", "STOP"); !errors.Is(err, app.ErrPhone) {
			t.Fatalf("%v", err)
		}
		if _, err := h.svc.RecordReplyKeyword(ctx, m.tenant, "fax", "+254700000301", "STOP"); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("%v", err)
		}
	})

	if strings.Contains(h.logs.String(), "700000301") {
		t.Fatal("a phone number reached the logs")
	}
}

func TestConsent_Suppressions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m, other := h.manager(), h.manager()

	s, created, err := h.svc.AddSuppression(ctx, m.user, m.tenant, app.SuppressionInput{Phone: "0700000401", Reason: "complaint", Note: "called to complain"})
	if err != nil || !created || s.Channel != "" || s.Phone != "+254700000401" {
		t.Fatalf("AddSuppression: %+v, %v, %v", s, created, err)
	}
	_, created, err = h.svc.AddSuppression(ctx, m.user, m.tenant, app.SuppressionInput{Phone: "+254700000401", Reason: "other"})
	if err != nil || created {
		t.Fatalf("again: created %v, %v", created, err)
	}
	if _, _, err := h.svc.AddSuppression(ctx, m.user, m.tenant, app.SuppressionInput{Phone: "0700000401", Channel: "sms", Reason: "client_request"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.AddSuppression(ctx, m.user, m.tenant, app.SuppressionInput{Phone: "0700000402", Channel: "whatsapp", Reason: "invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000401", Channel: "sms", Purpose: "transactional", Status: "granted"}); err != nil {
		t.Fatal(err)
	}

	t.Run("a suppression beats a grant, on every channel", func(t *testing.T) {
		st, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000401")
		for _, d := range st.Decisions {
			if d.Allowed || d.Reason != "suppressed" {
				t.Fatalf("%+v", d)
			}
		}
		if len(st.Suppressions) != 2 {
			t.Fatalf("suppressions %+v", st.Suppressions)
		}
	})

	t.Run("one channel only", func(t *testing.T) {
		st, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000402")
		if d := decision(t, st, "whatsapp", "transactional"); d.Reason != "suppressed" {
			t.Fatalf("%+v", d)
		}
		if d := decision(t, st, "sms", "promotional"); !d.Allowed {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("listed by number, page by page; another account sees none", func(t *testing.T) {
		var got []string
		cursor := ""
		for {
			page, next, err := h.svc.ListSuppressions(ctx, m.user, m.tenant, app.Page{Cursor: cursor, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range page {
				got = append(got, s.Phone+"/"+s.Channel)
			}
			if next == "" {
				break
			}
			cursor = next
		}
		if fmt.Sprint(got) != "[+254700000401/ +254700000401/sms +254700000402/whatsapp]" {
			t.Fatalf("listed %v", got)
		}
		theirs, _, err := h.svc.ListSuppressions(ctx, other.user, other.tenant, app.Page{})
		if err != nil || len(theirs) != 0 {
			t.Fatalf("another account: %v, %v", theirs, err)
		}
		if _, _, err := h.svc.ListSuppressions(ctx, m.user, m.tenant, app.Page{Cursor: "!!"}); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("a bad cursor: %v", err)
		}
	})

	t.Run("removed by number and channel", func(t *testing.T) {
		if err := h.svc.RemoveSuppression(ctx, m.user, m.tenant, "0700000401", ""); err != nil {
			t.Fatal(err)
		}
		st, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000401")
		if d := decision(t, st, "whatsapp", "promotional"); !d.Allowed {
			t.Fatalf("still blocked: %+v", d)
		}
		if d := decision(t, st, "sms", "promotional"); d.Reason != "suppressed" {
			t.Fatalf("sms: %+v", d)
		}
		if err := h.svc.RemoveSuppression(ctx, m.user, m.tenant, "0700000401", ""); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("removing it again: %v", err)
		}
		if err := h.svc.RemoveSuppression(ctx, other.user, other.tenant, "0700000401", "sms"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("another account removing it: %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for name, in := range map[string]app.SuppressionInput{
			"an unknown reason":  {Phone: "0700000403", Reason: "spite"},
			"an unknown channel": {Phone: "0700000403", Channel: "fax", Reason: "other"},
			"a long note":        {Phone: "0700000403", Reason: "other", Note: strings.Repeat("n", 501)},
		} {
			if _, _, err := h.svc.AddSuppression(ctx, m.user, m.tenant, in); !errors.Is(err, app.ErrValidation) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, _, err := h.svc.AddSuppression(ctx, m.user, m.tenant, app.SuppressionInput{Phone: "07", Reason: "other"}); !errors.Is(err, app.ErrPhone) {
			t.Errorf("a short number: %v", err)
		}
	})
}

func TestConsent_PreferenceCentre(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	contact, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: "0700000501"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := h.svc.IssuePreferenceLink(ctx, m.user, m.tenant, "0700 000 501")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(link.URL, "https://api.vaspro.test/p/")
	if len(token) != 12 || !link.ExpiresAt.Equal(h.clk.Now().Add(90*24*time.Hour)) {
		t.Fatalf("link %+v", link)
	}
	if n := h.count(t, `SELECT count(*) FROM preference_links WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, token); n != 1 {
		t.Fatal("the token isn't stored as its hash")
	}
	if n := h.count(t, `SELECT count(*) FROM preference_links WHERE encode(token_hash, 'escape') LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("the token is stored as itself")
	}

	t.Run("the page shows the masked number and the channels", func(t *testing.T) {
		p, err := h.svc.GetPreferences(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if p.MaskedPhone != "+254 7•• ••• 501" || len(p.Channels) != 2 || p.Channels[0].Channel != "sms" || !p.Channels[0].Receiving || !p.Channels[1].Receiving {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("turning SMS off is the person's withdrawal", func(t *testing.T) {
		h.clk.Advance(time.Hour)
		p, err := h.svc.UpdatePreferences(ctx, token, map[string]bool{"sms": false})
		if err != nil || p.Channels[0].Receiving || !p.Channels[1].Receiving {
			t.Fatalf("%+v, %v", p, err)
		}
		st, _ := h.svc.LookupConsent(ctx, m.user, m.tenant, "0700000501")
		if d := decision(t, st, "sms", "promotional"); d.Reason != "opted_out" || st.Consents[0].Actor != "subject" || st.Consents[0].Source != "preference_centre" {
			t.Fatalf("%+v %+v", d, st.Consents)
		}
		evs := h.events(t, "consent.withdrawn")
		if len(evs) != 1 || evs[0]["contact_id"] != contact.ID.String() || evs[0]["source"] != "preference_centre" {
			t.Fatalf("events %v", evs)
		}
		if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000501", Channel: "sms", Purpose: "promotional", Status: "granted"}); !errors.Is(err, app.ErrSubjectWithdrew) {
			t.Fatalf("the client granting over it: %v", err)
		}
		if n := h.count(t, `SELECT count(*) FROM preference_links WHERE last_used_at = $1`, h.clk.Now()); n != 1 {
			t.Fatal("last_used_at not stamped")
		}
	})

	t.Run("and back on", func(t *testing.T) {
		p, err := h.svc.UpdatePreferences(ctx, token, map[string]bool{"sms": true, "whatsapp": true})
		if err != nil || !p.Channels[0].Receiving {
			t.Fatalf("%+v, %v", p, err)
		}
	})

	t.Run("refused changes", func(t *testing.T) {
		for name, change := range map[string]map[string]bool{
			"nothing":            {},
			"an email channel":   {"email": false},
			"an unknown channel": {"fax": false},
		} {
			if _, err := h.svc.UpdatePreferences(ctx, token, change); !errors.Is(err, app.ErrValidation) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("tampered, malformed and unknown links find nothing", func(t *testing.T) {
		for i := range token {
			b := []byte(token)
			if b[i] == 'a' {
				b[i] = 'b'
			} else {
				b[i] = 'a'
			}
			if _, err := h.svc.GetPreferences(ctx, string(b)); !errors.Is(err, app.ErrNotFound) {
				t.Fatalf("changed at %d: %v", i, err)
			}
		}
		for _, bad := range []string{"", token[:11], token + "x", "../../etc/pa", strings.ToUpper(token) + "!"} {
			if _, err := h.svc.GetPreferences(ctx, bad); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%q: %v", bad, err)
			}
			if _, err := h.svc.UpdatePreferences(ctx, bad, map[string]bool{"sms": false}); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%q: %v", bad, err)
			}
		}
	})

	t.Run("issuing again replaces the link", func(t *testing.T) {
		again, err := h.svc.IssuePreferenceLink(ctx, m.user, m.tenant, "+254700000501")
		if err != nil || again.URL == link.URL {
			t.Fatalf("%+v, %v", again, err)
		}
		if _, err := h.svc.GetPreferences(ctx, token); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("the old link: %v", err)
		}
		token = strings.TrimPrefix(again.URL, "https://api.vaspro.test/p/")
		if _, err := h.svc.GetPreferences(ctx, token); err != nil {
			t.Fatalf("the new link: %v", err)
		}
	})

	t.Run("a link expires after 90 days", func(t *testing.T) {
		h.clk.Advance(90*24*time.Hour - time.Second)
		if _, err := h.svc.GetPreferences(ctx, token); err != nil {
			t.Fatalf("a second before: %v", err)
		}
		h.clk.Advance(time.Second)
		if _, err := h.svc.GetPreferences(ctx, token); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("at 90 days: %v", err)
		}
		if _, err := h.svc.UpdatePreferences(ctx, token, map[string]bool{"sms": false}); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("changing at 90 days: %v", err)
		}
	})

	t.Run("a link issued for messaging needs no user", func(t *testing.T) {
		l, err := h.svc.IssuePreferenceLinkFor(ctx, m.tenant, "+254700000502")
		if err != nil || !strings.HasPrefix(l.URL, "https://api.vaspro.test/p/") {
			t.Fatalf("%+v, %v", l, err)
		}
	})

	if strings.Contains(h.logs.String(), "700000501") || strings.Contains(h.logs.String(), token) {
		t.Fatal("a number or token reached the logs")
	}
}

func TestConsent_Who(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m, other := h.manager(), h.manager()
	if _, err := h.svc.RecordConsent(ctx, m.user, m.tenant, app.ConsentInput{Phone: "0700000601", Channel: "sms", Purpose: "promotional", Status: "withdrawn"}); err != nil {
		t.Fatal(err)
	}
	reader := member{user: uuid.New(), tenant: m.tenant}
	h.accounts.Grant(reader.user, reader.tenant, app.PermRead)

	if s, err := h.svc.LookupConsent(ctx, reader.user, reader.tenant, "0700000601"); err != nil || len(s.Consents) != 1 {
		t.Fatalf("a reader's lookup: %+v, %v", s, err)
	}
	if _, _, err := h.svc.ListSuppressions(ctx, reader.user, reader.tenant, app.Page{}); err != nil {
		t.Fatalf("a reader's list: %v", err)
	}
	for what, err := range map[string]error{
		"record": func() error {
			_, err := h.svc.RecordConsent(ctx, reader.user, reader.tenant, app.ConsentInput{Phone: "0700000601", Channel: "sms", Purpose: "promotional", Status: "granted"})
			return err
		}(),
		"suppress": func() error {
			_, _, err := h.svc.AddSuppression(ctx, reader.user, reader.tenant, app.SuppressionInput{Phone: "0700000601", Reason: "other"})
			return err
		}(),
		"unsuppress": h.svc.RemoveSuppression(ctx, reader.user, reader.tenant, "0700000601", ""),
		"issue a link": func() error {
			_, err := h.svc.IssuePreferenceLink(ctx, reader.user, reader.tenant, "0700000601")
			return err
		}(),
	} {
		if !errors.Is(err, app.ErrPermissionDenied) {
			t.Errorf("a reader may %s: %v", what, err)
		}
	}
	if s, err := h.svc.LookupConsent(ctx, other.user, other.tenant, "0700000601"); err != nil || len(s.Consents) != 0 || !decision(t, s, "sms", "promotional").Allowed {
		t.Fatalf("another account sees %+v, %v", s, err)
	}
	if _, err := h.svc.LookupConsent(ctx, uuid.New(), m.tenant, "0700000601"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("a stranger: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM consents`); n != 1 {
		t.Fatalf("%d consents", n)
	}
	_ = domain.Channels
}
