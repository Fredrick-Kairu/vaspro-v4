package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// ErrSubjectWithdrew: the person withdrew this consent themselves, and
// only they can give it back (VAL-4026).
var ErrSubjectWithdrew = errors.New("app: the person withdrew this consent themselves; only they can give it back")

// PreferenceLinkLifetime is how long a preference link works (data model
// §10).
const PreferenceLinkLifetime = 90 * 24 * time.Hour

// preferenceChannels are the channels the preference page offers: those
// VASPRO sends promotional messages on. Email, Telegram and RCS join
// when those channels are built.
var preferenceChannels = []string{"sms", "whatsapp"}

// suppressionReasons are suppressions.reason's values.
var suppressionReasons = map[string]bool{"client_request": true, "complaint": true, "invalid": true, "other": true}

// ConsentInput records the client's consent for one number, channel and
// purpose.
type ConsentInput struct{ Phone, Channel, Purpose, Status, Evidence string }

// Consent is one recorded consent.
type Consent struct {
	Channel, Purpose, Status, Actor, Source, Evidence string
	RecordedAt                                        time.Time
}

// ConsentEvent is one change in a number's consent history.
type ConsentEvent struct {
	Channel, Purpose, Status, Actor, Source, Evidence string
	RecordedBy                                        *uuid.UUID
	OccurredAt                                        time.Time
}

// ConsentDecision is whether a send on a channel for a purpose may go.
type ConsentDecision struct {
	Channel, Purpose string
	Allowed          bool
	Reason           string
}

// Suppression is a number on the client's do-not-contact list; Channel
// "" is every channel.
type Suppression struct {
	Phone, Channel, Reason, Note string
	CreatedAt                    time.Time
}

// ConsentState is everything about one number's consent in an account.
type ConsentState struct {
	Phone        string
	ContactID    *uuid.UUID
	Consents     []Consent
	Suppressions []Suppression
	Decisions    []ConsentDecision // every channel × purpose
	History      []ConsentEvent    // newest first, at most 50
}

// SuppressionInput adds a number to the client's list; Channel "" is
// every channel.
type SuppressionInput struct{ Phone, Channel, Reason, Note string }

// PreferenceLink is a preference page's address; its token is shown
// only here.
type PreferenceLink struct {
	URL       string
	ExpiresAt time.Time
}

// ChannelPreference is whether the person receives promotional messages
// on a channel.
type ChannelPreference struct {
	Channel   string
	Receiving bool
}

// Preferences is what the preference page shows: never the whole number.
type Preferences struct {
	MaskedPhone string
	Channels    []ChannelPreference
	ExpiresAt   time.Time
}

// change is one consent change, by the client or the person.
type change struct {
	tenant                          uuid.UUID
	phone, channel, purpose, status string
	actor, source, evidence         string
	recordedBy                      *uuid.UUID
}

func toConsent(c store.ContactsConsent) Consent {
	return Consent{
		Channel: string(c.Channel), Purpose: string(c.Purpose), Status: c.Status, Actor: c.Actor, Source: c.Source,
		Evidence: c.Evidence.String, RecordedAt: c.RecordedAt,
	}
}

// applyChange writes a consent and its history row, and publishes
// consent.withdrawn when the number's consent becomes withdrawn, all in
// the caller's tenant transaction. The row is locked first, so two
// changes to one consent take turns.
func (s *Service) applyChange(ctx context.Context, q *store.Queries, tx pgx.Tx, c change) (store.ContactsConsent, error) {
	key := store.GetConsentForUpdateParams{PhoneE164: c.phone, Channel: store.ContactsChannel(c.channel), Purpose: store.ContactsPurpose(c.purpose)}
	before := ""
	current, err := q.GetConsentForUpdate(ctx, key)
	switch {
	case err == nil:
		before = current.Status
		if c.actor == "client" {
			write, err := domain.ClientConsentChange(current.Actor, current.Status, c.status)
			if errors.Is(err, domain.ErrSubjectWithdrew) {
				return store.ContactsConsent{}, ErrSubjectWithdrew
			}
			if !write {
				return current, nil
			}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return store.ContactsConsent{}, err
	}
	at := s.clk.Now()
	by := pgtype.UUID{}
	if c.recordedBy != nil {
		by = pgtype.UUID{Bytes: *c.recordedBy, Valid: true}
	}
	row, err := q.UpsertConsent(ctx, store.UpsertConsentParams{
		TenantID: c.tenant, PhoneE164: c.phone, Channel: key.Channel, Purpose: key.Purpose, Status: c.status,
		Actor: c.actor, Source: c.source, Evidence: text(c.evidence), RecordedBy: by, RecordedAt: at,
	})
	if err != nil {
		return store.ContactsConsent{}, err
	}
	id, err := ids.New()
	if err != nil {
		return store.ContactsConsent{}, err
	}
	if err := q.InsertConsentEvent(ctx, store.InsertConsentEventParams{
		ID: id, TenantID: c.tenant, PhoneE164: c.phone, Channel: key.Channel, Purpose: key.Purpose, Status: c.status,
		Actor: c.actor, Source: c.source, Evidence: text(c.evidence), RecordedBy: by, OccurredAt: at,
	}); err != nil {
		return store.ContactsConsent{}, err
	}
	if !domain.WithdrawalIsNews(before, c.status) {
		return row, nil
	}
	data := map[string]any{
		"tenant_id": c.tenant, "channel": c.channel, "purpose": c.purpose, "actor": c.actor, "source": c.source, "withdrawn_at": at.UTC(),
	}
	if contact, err := q.GetContactByPhone(ctx, c.phone); err == nil {
		data["contact_id"] = contact.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.ContactsConsent{}, err
	}
	env, err := events.New(ids.MustNew(), "consent.withdrawn", 1, c.tenant, at, "", data)
	if err != nil {
		return store.ContactsConsent{}, err
	}
	return row, outbox.Write(ctx, tx, "consent", id, env)
}

func checkChannelPurpose(channel, purpose string) error {
	if !domain.ValidChannel(channel) || !domain.ValidPurpose(purpose) {
		return fmt.Errorf("%w: channel is one of %s and purpose one of %s", ErrValidation,
			strings.Join(domain.Channels, ", "), strings.Join(domain.Purposes, ", "))
	}
	return nil
}

// normalize reads a number in country's pack, as ErrPhone with the
// reason when it isn't one.
func (s *Service) normalize(country, raw string) (string, error) {
	rules, err := s.phoneRules(country)
	if err != nil {
		return "", err
	}
	phone, err := domain.NormalizePhone(rules, raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPhone, err)
	}
	return phone, nil
}

// RecordConsent records the client's grant or withdrawal (source api).
// A person's own withdrawal can't be granted over (ErrSubjectWithdrew);
// withdrawing it again changes nothing.
func (s *Service) RecordConsent(ctx context.Context, user, tenant uuid.UUID, in ConsentInput) (Consent, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermManage)
	if err != nil {
		return Consent{}, err
	}
	if err := checkChannelPurpose(in.Channel, in.Purpose); err != nil {
		return Consent{}, err
	}
	if in.Status != domain.ConsentGranted && in.Status != domain.ConsentWithdrawn {
		return Consent{}, fmt.Errorf("%w: status is granted or withdrawn", ErrValidation)
	}
	if utf8.RuneCountInString(in.Evidence) > 500 {
		return Consent{}, fmt.Errorf("%w: evidence is at most 500 characters", ErrValidation)
	}
	phone, err := s.normalize(country, in.Phone)
	if err != nil {
		return Consent{}, err
	}
	var out store.ContactsConsent
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.applyChange(ctx, store.New(tx), tx, change{
			tenant: tenant, phone: phone, channel: in.Channel, purpose: in.Purpose, status: in.Status,
			actor: "client", source: "api", evidence: strings.TrimSpace(in.Evidence), recordedBy: &user,
		})
		return err
	})
	return toConsent(out), err
}

// requiresOptIn is the pack's promotional rule; a pack without an
// opt_out block fails closed, as opt-in.
func requiresOptIn(p *packs.Pack) bool { return p.OptOut == nil || p.OptOut.PromotionalRequiresOptIn }

// LookupConsent is a number's consents, suppressions, decisions for
// every channel and purpose, and recent history.
func (s *Service) LookupConsent(ctx context.Context, user, tenant uuid.UUID, raw string) (ConsentState, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermRead)
	if err != nil {
		return ConsentState{}, err
	}
	phone, err := s.normalize(country, raw)
	if err != nil {
		return ConsentState{}, err
	}
	out := ConsentState{Phone: phone}
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if c, err := q.GetContactByPhone(ctx, phone); err == nil {
			out.ContactID = &c.ID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		consents, err := q.ListConsentsForPhone(ctx, phone)
		if err != nil {
			return err
		}
		supps, err := q.ListSuppressionsForPhone(ctx, phone)
		if err != nil {
			return err
		}
		history, err := q.ListRecentConsentEvents(ctx, store.ListRecentConsentEventsParams{PhoneE164: phone, MaxRows: 50})
		if err != nil {
			return err
		}
		for _, c := range consents {
			out.Consents = append(out.Consents, toConsent(c))
		}
		for _, sp := range supps {
			out.Suppressions = append(out.Suppressions, toSuppression(sp))
		}
		for _, e := range history {
			ev := ConsentEvent{
				Channel: string(e.Channel), Purpose: string(e.Purpose), Status: e.Status, Actor: e.Actor, Source: e.Source,
				Evidence: e.Evidence.String, OccurredAt: e.OccurredAt,
			}
			if e.RecordedBy.Valid {
				by := uuid.UUID(e.RecordedBy.Bytes)
				ev.RecordedBy = &by
			}
			out.History = append(out.History, ev)
		}
		return nil
	})
	if err != nil {
		return ConsentState{}, err
	}
	out.Decisions = decisions(out, requiresOptIn(s.packs[country]))
	return out, nil
}

func decisions(st ConsentState, optIn bool) []ConsentDecision {
	supps := make([]string, len(st.Suppressions))
	for i, sp := range st.Suppressions {
		supps[i] = sp.Channel
	}
	status := map[string]string{}
	for _, c := range st.Consents {
		status[c.Channel+"/"+c.Purpose] = c.Status
	}
	var out []ConsentDecision
	for _, ch := range domain.Channels {
		for _, p := range domain.Purposes {
			d := domain.DecideConsent(domain.DecisionInput{
				ValidNumber: true, Channel: ch, Purpose: p, Status: status[ch+"/"+p], Suppressions: supps, RequiresOptIn: optIn,
			})
			out = append(out, ConsentDecision{Channel: ch, Purpose: p, Allowed: d.Allowed, Reason: d.Reason})
		}
	}
	return out
}

// soleCountry is the one loaded pack's country, for work done for no
// user (reply keywords, the preference page), like imports: a second
// country needs the account's country from accounts (data model §10).
func (s *Service) soleCountry() (string, error) {
	if len(s.packs) != 1 {
		return "", errors.New("app: work without a user reads the one loaded pack; more than one needs the account's country")
	}
	for code := range s.packs {
		return code, nil
	}
	return "", nil
}

// RecordReplyKeyword records a reply's stop or start word as the
// person's own choice for promotional messages on the channel it came
// in on, and says which it was ("" for none, writing nothing).
// Messaging calls it over gRPC (task 6.5) for the client it decided
// the reply belongs to.
func (s *Service) RecordReplyKeyword(ctx context.Context, tenant uuid.UUID, channel, rawPhone, text string) (string, error) {
	if !domain.ValidChannel(channel) {
		return "", fmt.Errorf("%w: unknown channel", ErrValidation)
	}
	country, err := s.soleCountry()
	if err != nil {
		return "", err
	}
	phone, err := s.normalize(country, rawPhone)
	if err != nil {
		return "", err
	}
	rules := domain.KeywordRules{}
	if p := s.packs[country]; p.OptOut != nil {
		rules = domain.KeywordRules{Stop: p.OptOut.Stop, Start: p.OptOut.Start}
	}
	matched := domain.MatchKeyword(rules, text)
	if matched == "" {
		return "", nil
	}
	status := domain.ConsentWithdrawn
	if matched == "start" {
		status = domain.ConsentGranted
	}
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.applyChange(ctx, store.New(tx), tx, change{
			tenant: tenant, phone: phone, channel: channel, purpose: domain.PurposePromotional, status: status,
			actor: "subject", source: "keyword",
		})
		return err
	})
	if err != nil {
		return "", err
	}
	s.log.InfoContext(ctx, "contacts: reply keyword recorded", "tenant_id", tenant, "channel", channel, "keyword", matched)
	return matched, nil
}

func toSuppression(sp store.ContactsSuppression) Suppression {
	ch := ""
	if sp.Channel.Valid {
		ch = string(sp.Channel.ContactsChannel)
	}
	return Suppression{Phone: sp.PhoneE164, Channel: ch, Reason: sp.Reason, Note: sp.Note.String, CreatedAt: sp.CreatedAt}
}

func nullChannel(ch string) store.NullContactsChannel {
	return store.NullContactsChannel{ContactsChannel: store.ContactsChannel(ch), Valid: ch != ""}
}

// AddSuppression puts a number on the client's do-not-contact list, for
// one channel or ("" ) every channel; created is false when it was
// already there, and the existing entry is returned.
func (s *Service) AddSuppression(ctx context.Context, user, tenant uuid.UUID, in SuppressionInput) (Suppression, bool, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermManage)
	if err != nil {
		return Suppression{}, false, err
	}
	if (in.Channel != "" && !domain.ValidChannel(in.Channel)) || !suppressionReasons[in.Reason] || utf8.RuneCountInString(in.Note) > 500 {
		return Suppression{}, false, fmt.Errorf("%w: a known channel (or none, for every channel), a reason of client_request, complaint, invalid or other, and a note of at most 500 characters", ErrValidation)
	}
	phone, err := s.normalize(country, in.Phone)
	if err != nil {
		return Suppression{}, false, err
	}
	var row store.ContactsSuppression
	created := false
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.AddSuppression(ctx, store.AddSuppressionParams{
			TenantID: tenant, PhoneE164: phone, Channel: nullChannel(in.Channel), Reason: in.Reason, Note: text(strings.TrimSpace(in.Note)), CreatedBy: user,
		})
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row, err = q.GetSuppression(ctx, store.GetSuppressionParams{PhoneE164: phone, Channel: nullChannel(in.Channel)})
		return err
	})
	return toSuppression(row), created, err
}

// ListSuppressions pages the client's list by number, then channel.
func (s *Service) ListSuppressions(ctx context.Context, user, tenant uuid.UUID, page Page) ([]Suppression, string, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, "", err
	}
	size, err := page.size()
	if err != nil {
		return nil, "", err
	}
	params := store.ListSuppressionsParams{PageSize: size + 1}
	if page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page.Cursor)
		phone, ch, ok := strings.Cut(string(raw), "/")
		if err != nil || !ok || !domain.ValidE164(phone) || (ch != "" && !domain.ValidChannel(ch)) {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrValidation)
		}
		params.AfterPhone, params.AfterChannel = pgtype.Text{String: phone, Valid: true}, ch
	}
	var rows []store.ContactsSuppression
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListSuppressions(ctx, params)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > int(size) {
		rows = rows[:size]
		last := toSuppression(rows[len(rows)-1])
		next = base64.RawURLEncoding.EncodeToString([]byte(last.Phone + "/" + last.Channel))
	}
	out := make([]Suppression, len(rows))
	for i, r := range rows {
		out[i] = toSuppression(r)
	}
	return out, next, nil
}

// RemoveSuppression takes a number off the list for one channel, or
// ("") the every-channel entry.
func (s *Service) RemoveSuppression(ctx context.Context, user, tenant uuid.UUID, rawPhone, channel string) error {
	country, err := s.requireTenant(ctx, user, tenant, PermManage)
	if err != nil {
		return err
	}
	if channel != "" && !domain.ValidChannel(channel) {
		return fmt.Errorf("%w: unknown channel", ErrValidation)
	}
	phone, err := s.normalize(country, rawPhone)
	if err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteSuppression(ctx, store.DeleteSuppressionParams{PhoneE164: phone, Channel: nullChannel(channel)})
		if err == nil && n == 0 {
			return fmt.Errorf("%w: suppression", ErrNotFound)
		}
		return err
	})
}

// IssuePreferenceLink issues a number's preference link for the client,
// replacing any earlier one.
func (s *Service) IssuePreferenceLink(ctx context.Context, user, tenant uuid.UUID, rawPhone string) (PreferenceLink, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermManage)
	if err != nil {
		return PreferenceLink{}, err
	}
	phone, err := s.normalize(country, rawPhone)
	if err != nil {
		return PreferenceLink{}, err
	}
	return s.issueLink(ctx, tenant, phone)
}

// IssuePreferenceLinkFor issues a link for messaging, which builds
// promotional messages for a client (gRPC, task 6.5): no user.
func (s *Service) IssuePreferenceLinkFor(ctx context.Context, tenant uuid.UUID, rawPhone string) (PreferenceLink, error) {
	country, err := s.soleCountry()
	if err != nil {
		return PreferenceLink{}, err
	}
	phone, err := s.normalize(country, rawPhone)
	if err != nil {
		return PreferenceLink{}, err
	}
	return s.issueLink(ctx, tenant, phone)
}

func (s *Service) issueLink(ctx context.Context, tenant uuid.UUID, phone string) (PreferenceLink, error) {
	token, err := domain.NewPreferenceToken(rand.Reader)
	if err != nil {
		return PreferenceLink{}, err
	}
	hash, _ := domain.HashPreferenceToken(token) // a new token always has its shape
	now := s.clk.Now()
	expires := now.Add(PreferenceLinkLifetime)
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).UpsertPreferenceLink(ctx, store.UpsertPreferenceLinkParams{
			TenantID: tenant, PhoneE164: phone, TokenHash: hash, CreatedAt: now, ExpiresAt: expires,
		})
		return err
	})
	if err != nil {
		return PreferenceLink{}, err
	}
	return PreferenceLink{URL: s.baseURL + "/p/" + token, ExpiresAt: expires}, nil
}

// findLink finds a working link by its token, as contacts_system; a
// token of the wrong shape, unknown, replaced or expired is ErrNotFound.
func (s *Service) findLink(ctx context.Context, token string) (store.ContactsPreferenceLink, error) {
	hash, ok := domain.HashPreferenceToken(token)
	if !ok {
		return store.ContactsPreferenceLink{}, fmt.Errorf("%w: preference link", ErrNotFound)
	}
	var link store.ContactsPreferenceLink
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		link, err = store.New(tx).FindPreferenceLink(ctx, hash)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !s.clk.Now().Before(link.ExpiresAt) {
		return store.ContactsPreferenceLink{}, fmt.Errorf("%w: preference link", ErrNotFound)
	}
	return link, err
}

// GetPreferences is what a preference page shows.
func (s *Service) GetPreferences(ctx context.Context, token string) (Preferences, error) {
	link, err := s.findLink(ctx, token)
	if err != nil {
		return Preferences{}, err
	}
	if err := s.touchLink(ctx, link); err != nil {
		return Preferences{}, err
	}
	return s.preferences(ctx, link)
}

// touchLink stamps a link's last use, as contacts_system.
func (s *Service) touchLink(ctx context.Context, link store.ContactsPreferenceLink) error {
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).TouchPreferenceLink(ctx, store.TouchPreferenceLinkParams{At: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true}, TokenHash: link.TokenHash})
	})
}

func (s *Service) preferences(ctx context.Context, link store.ContactsPreferenceLink) (Preferences, error) {
	country, err := s.soleCountry()
	if err != nil {
		return Preferences{}, err
	}
	st := ConsentState{Phone: link.PhoneE164}
	err = s.pool.WithTenantTx(ctx, link.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		consents, err := q.ListConsentsForPhone(ctx, link.PhoneE164)
		if err != nil {
			return err
		}
		supps, err := q.ListSuppressionsForPhone(ctx, link.PhoneE164)
		for _, c := range consents {
			st.Consents = append(st.Consents, toConsent(c))
		}
		for _, sp := range supps {
			st.Suppressions = append(st.Suppressions, toSuppression(sp))
		}
		return err
	})
	if err != nil {
		return Preferences{}, err
	}
	optIn := requiresOptIn(s.packs[country])
	out := Preferences{MaskedPhone: maskPhone(s.packs[country].Numbering.CountryCode, link.PhoneE164), ExpiresAt: link.ExpiresAt}
	all := decisions(st, optIn)
	for _, ch := range preferenceChannels {
		allowed := false
		for _, d := range all {
			if d.Channel == ch && d.Purpose == domain.PurposePromotional {
				allowed = d.Allowed
			}
		}
		out.Channels = append(out.Channels, ChannelPreference{Channel: ch, Receiving: allowed})
	}
	return out, nil
}

// UpdatePreferences records the person's choice per channel, for
// promotional messages, from their preference page.
func (s *Service) UpdatePreferences(ctx context.Context, token string, receiving map[string]bool) (Preferences, error) {
	link, err := s.findLink(ctx, token)
	if err != nil {
		return Preferences{}, err
	}
	if len(receiving) == 0 {
		return Preferences{}, fmt.Errorf("%w: choose at least one channel", ErrValidation)
	}
	for ch := range receiving {
		if !slices.Contains(preferenceChannels, ch) {
			return Preferences{}, fmt.Errorf("%w: the channels are %s", ErrValidation, strings.Join(preferenceChannels, " and "))
		}
	}
	err = s.pool.WithTenantTx(ctx, link.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for _, ch := range preferenceChannels {
			on, ok := receiving[ch]
			if !ok {
				continue
			}
			status := domain.ConsentWithdrawn
			if on {
				status = domain.ConsentGranted
			}
			current, err := q.GetConsent(ctx, store.GetConsentParams{PhoneE164: link.PhoneE164, Channel: store.ContactsChannel(ch), Purpose: domain.PurposePromotional})
			if err == nil && current.Status == status && current.Actor == "subject" {
				continue
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := s.applyChange(ctx, q, tx, change{
				tenant: link.TenantID, phone: link.PhoneE164, channel: ch, purpose: domain.PurposePromotional, status: status,
				actor: "subject", source: "preference_centre",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Preferences{}, err
	}
	if err := s.touchLink(ctx, link); err != nil {
		return Preferences{}, err
	}
	return s.preferences(ctx, link)
}

// maskPhone shows a number's country code, first digit and last three,
// grouped in threes: +254 7•• ••• 501.
func maskPhone(countryCode, phone string) string {
	digits := strings.TrimPrefix(phone, "+")
	cc := countryCode
	if !strings.HasPrefix(digits, cc) {
		cc = digits[:min(3, len(digits)-4)]
	}
	nsn := digits[len(cc):]
	masked := nsn[:1] + strings.Repeat("•", len(nsn)-4) + nsn[len(nsn)-3:]
	runes := []rune(masked)
	var groups []string
	for len(runes) > 3 {
		groups, runes = append(groups, string(runes[:3])), runes[3:]
	}
	return "+" + cc + " " + strings.Join(append(groups, string(runes)), " ")
}
