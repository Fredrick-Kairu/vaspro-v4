// Package email fakes identity's adapters.EmailSender for tests —
// records what would have been sent instead of actually sending it
// (.claude/rules/go.md: "Adapters are tested against fakes").
package email

import (
	"context"
	"sync"
	"time"
)

// Kind distinguishes which EmailSender method a Sent came from, since
// Sender records both through one list.
const (
	KindVerification  = "verification"
	KindPasswordReset = "password_reset"
	KindStepUp        = "step_up"
	KindLoginAlert    = "login_alert"
	KindInvitation    = "invitation"
)

// Sent is one recorded call to Sender. Code is set for the three
// code-based kinds; IP/OccurredAt are set for KindLoginAlert, and
// AccountName/InviterName for KindInvitation, instead.
type Sent struct {
	Kind        string
	To          string
	FullName    string
	Code        string
	IP          string
	OccurredAt  time.Time
	AccountName string
	InviterName string
}

// Sender is a fake adapters.EmailSender: thread-safe, keeps every call
// it was given.
type Sender struct {
	mu   sync.Mutex
	sent []Sent
}

func (s *Sender) SendVerificationCode(_ context.Context, to, fullName, code string) error {
	s.record(Sent{Kind: KindVerification, To: to, FullName: fullName, Code: code})
	return nil
}

func (s *Sender) SendPasswordResetCode(_ context.Context, to, fullName, code string) error {
	s.record(Sent{Kind: KindPasswordReset, To: to, FullName: fullName, Code: code})
	return nil
}

func (s *Sender) SendStepUpCode(_ context.Context, to, fullName, code string) error {
	s.record(Sent{Kind: KindStepUp, To: to, FullName: fullName, Code: code})
	return nil
}

func (s *Sender) SendLoginAlert(_ context.Context, to, fullName, ip string, occurredAt time.Time) error {
	s.record(Sent{Kind: KindLoginAlert, To: to, FullName: fullName, IP: ip, OccurredAt: occurredAt})
	return nil
}

func (s *Sender) SendInvitation(_ context.Context, to, accountName, inviterName string) error {
	s.record(Sent{Kind: KindInvitation, To: to, AccountName: accountName, InviterName: inviterName})
	return nil
}

func (s *Sender) record(sent Sent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sent)
}

// All returns every call recorded so far, oldest first.
func (s *Sender) All() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sent, len(s.sent))
	copy(out, s.sent)
	return out
}

// Last returns the most recent call, or false if none happened yet.
func (s *Sender) Last() (Sent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return Sent{}, false
	}
	return s.sent[len(s.sent)-1], true
}

// LastOfKind returns the most recent call of the given Kind, or false
// if none happened yet.
func (s *Sender) LastOfKind(kind string) (Sent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sent) - 1; i >= 0; i-- {
		if s.sent[i].Kind == kind {
			return s.sent[i], true
		}
	}
	return Sent{}, false
}
