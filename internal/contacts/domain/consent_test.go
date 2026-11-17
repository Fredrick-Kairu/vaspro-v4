package domain_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// The data model §10 decision table, row by row.
func TestDecideConsent_Table(t *testing.T) {
	for _, c := range []struct {
		name string
		in   domain.DecisionInput
		want domain.Decision
	}{
		{"suppressed on this channel blocks transactional", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "transactional", Suppressions: []string{"sms"}, Status: "granted"}, domain.Decision{Reason: "suppressed"}},
		{"suppressed on every channel blocks promotional", domain.DecisionInput{ValidNumber: true, Channel: "whatsapp", Purpose: "promotional", Suppressions: []string{""}, Status: "granted"}, domain.Decision{Reason: "suppressed"}},
		{"suppressed elsewhere doesn't block", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional", Suppressions: []string{"whatsapp"}}, domain.Decision{Allowed: true}},
		{"withdrawn blocks transactional", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "transactional", Status: "withdrawn"}, domain.Decision{Reason: "opted_out"}},
		{"withdrawn blocks promotional", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional", Status: "withdrawn"}, domain.Decision{Reason: "opted_out"}},
		{"withdrawn blocks in an opt-in country", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional", Status: "withdrawn", RequiresOptIn: true}, domain.Decision{Reason: "opted_out"}},
		{"granted allows", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional", Status: "granted", RequiresOptIn: true}, domain.Decision{Allowed: true}},
		{"none allows transactional in an opt-in country", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "transactional", RequiresOptIn: true}, domain.Decision{Allowed: true}},
		{"none allows promotional in an opt-out country", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional"}, domain.Decision{Allowed: true}},
		{"none blocks promotional in an opt-in country", domain.DecisionInput{ValidNumber: true, Channel: "sms", Purpose: "promotional", RequiresOptIn: true}, domain.Decision{Reason: "no_consent"}},
		{"an invalid number, whatever else", domain.DecisionInput{Channel: "sms", Purpose: "transactional", Status: "granted"}, domain.Decision{Reason: "invalid_number"}},
		{"an invalid number before a suppression", domain.DecisionInput{Channel: "sms", Purpose: "promotional", Suppressions: []string{""}}, domain.Decision{Reason: "invalid_number"}},
	} {
		if got := domain.DecideConsent(c.in); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Every channel × purpose × recorded state × suppression × country
// model, against the table's rules stated once more.
func TestDecideConsent_EveryCombination(t *testing.T) {
	suppressions := map[string][]string{"none": nil, "this channel": {"THIS"}, "every channel": {""}, "another channel": {"OTHER"}}
	n := 0
	for _, channel := range domain.Channels {
		other := "sms"
		if channel == "sms" {
			other = "email"
		}
		for _, purpose := range domain.Purposes {
			for _, status := range []string{"", domain.ConsentGranted, domain.ConsentWithdrawn} {
				for sName, sup := range suppressions {
					for _, optIn := range []bool{false, true} {
						for _, valid := range []bool{true, false} {
							in := domain.DecisionInput{ValidNumber: valid, Channel: channel, Purpose: purpose, Status: status, RequiresOptIn: optIn}
							for _, s := range sup {
								in.Suppressions = append(in.Suppressions, strings.NewReplacer("THIS", channel, "OTHER", other).Replace(s))
							}
							want := domain.Decision{Allowed: true}
							switch {
							case !valid:
								want = domain.Decision{Reason: domain.ReasonInvalidNumber}
							case sName == "this channel" || sName == "every channel":
								want = domain.Decision{Reason: domain.ReasonSuppressed}
							case status == domain.ConsentWithdrawn:
								want = domain.Decision{Reason: domain.ReasonOptedOut}
							case status == "" && purpose == domain.PurposePromotional && optIn:
								want = domain.Decision{Reason: domain.ReasonNoConsent}
							}
							if got := domain.DecideConsent(in); got != want {
								t.Errorf("%+v: %+v, want %+v", in, got, want)
							}
							n++
						}
					}
				}
			}
		}
	}
	if n != 5*2*3*4*2*2 {
		t.Fatalf("%d combinations", n)
	}
}

func TestClientConsentChange(t *testing.T) {
	for _, c := range []struct {
		name                     string
		actor, status, newStatus string
		write                    bool
		err                      error
	}{
		{"nothing recorded: grant", "", "", "granted", true, nil},
		{"nothing recorded: withdraw", "", "", "withdrawn", true, nil},
		{"the client's grant: withdraw", "client", "granted", "withdrawn", true, nil},
		{"the client's withdrawal: grant", "client", "withdrawn", "granted", true, nil},
		{"the person's grant: the client may withdraw", "subject", "granted", "withdrawn", true, nil},
		{"the person's grant: grant again", "subject", "granted", "granted", true, nil},
		{"the person's withdrawal: the client can't grant", "subject", "withdrawn", "granted", false, domain.ErrSubjectWithdrew},
		{"the person's withdrawal: withdrawing again changes nothing", "subject", "withdrawn", "withdrawn", false, nil},
	} {
		write, err := domain.ClientConsentChange(c.actor, c.status, c.newStatus)
		if write != c.write || !errors.Is(err, c.err) {
			t.Errorf("%s: %v, %v; want %v, %v", c.name, write, err, c.write, c.err)
		}
	}
}

func TestWithdrawalIsNews(t *testing.T) {
	for _, c := range []struct {
		before, after string
		want          bool
	}{
		{"", "withdrawn", true}, {"granted", "withdrawn", true}, {"withdrawn", "withdrawn", false},
		{"", "granted", false}, {"withdrawn", "granted", false},
	} {
		if got := domain.WithdrawalIsNews(c.before, c.after); got != c.want {
			t.Errorf("%q → %q: %v", c.before, c.after, got)
		}
	}
}

// Keyword matching (data model §10): whole replies only, ignoring case,
// surrounding and repeated spaces and one trailing . , ! or ?.
func TestMatchKeyword(t *testing.T) {
	rules := domain.KeywordRules{Stop: []string{"STOP", "STOP ALL"}, Start: []string{"START"}}
	for text, want := range map[string]string{
		"STOP": "stop", "stop": "stop", " Stop. ": "stop", "STOP!": "stop", "stop?": "stop", "Stop,": "stop",
		"\tstop\n": "stop", "stop  all": "stop", " STOP ALL! ": "stop",
		"START": "start", "start.": "start", "Start": "start",
		"stopped": "", "please stop": "", "stop now": "", "STOP!!": "", "STOP.!": "", "!STOP": "", "": "", "   ": "",
		"STÖP": "", "S T O P": "", "stop-": "", "ST0P": "", "START STOP": "",
	} {
		if got := domain.MatchKeyword(rules, text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}

func TestPreferenceTokens(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok, err := domain.NewPreferenceToken(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 12 || seen[tok] {
			t.Fatalf("token %q (seen %v)", tok, seen[tok])
		}
		seen[tok] = true
		if h, ok := domain.HashPreferenceToken(tok); !ok || len(h) != 32 {
			t.Fatalf("hash of %q: %x, %v", tok, h, ok)
		}
	}
	fixed, err := domain.NewPreferenceToken(bytes.NewReader(bytes.Repeat([]byte{0, 61, 62, 255}, 10)))
	if err != nil {
		t.Fatal(err)
	}
	// 0 → '0', 61 → 'z'; 62 and above are refused rather than biasing.
	if fixed != "0z0z0z0z0z0z" {
		t.Fatalf("fixed source gave %q", fixed)
	}
	if _, err := domain.NewPreferenceToken(bytes.NewReader([]byte{1, 2})); err == nil {
		t.Fatal("a short source made a token")
	}
	a, _ := domain.HashPreferenceToken("abcdefABCDEF")
	b, _ := domain.HashPreferenceToken("abcdefABCDEf")
	if bytes.Equal(a, b) {
		t.Fatal("two tokens share a hash")
	}
	for _, bad := range []string{"", "abcdefABCDE", "abcdefABCDEFG", "abcdefABCDE!", "abcdefABCDE ", "abcdéfABCDE"} {
		if _, ok := domain.HashPreferenceToken(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestChannelsAndPurposes(t *testing.T) {
	if fmt.Sprint(domain.Channels) != "[sms whatsapp email telegram rcs]" || fmt.Sprint(domain.Purposes) != "[transactional promotional]" {
		t.Fatalf("%v %v", domain.Channels, domain.Purposes)
	}
	if !domain.ValidChannel("sms") || domain.ValidChannel("SMS") || domain.ValidChannel("") || !domain.ValidPurpose("promotional") || domain.ValidPurpose("marketing") {
		t.Fatal("channel or purpose check")
	}
}
