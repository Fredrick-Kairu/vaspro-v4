// Package email fakes identity's adapters.EmailSender for tests —
// records what would have been sent instead of actually sending it
// (.claude/rules/go.md: "Adapters are tested against fakes").
package email

import (
	"context"
	"sync"
)

// Sent is one recorded call to Sender.SendVerificationCode.
type Sent struct {
	To       string
	FullName string
	Code     string
}

// Sender is a fake adapters.EmailSender: thread-safe, keeps every call
// it was given.
type Sender struct {
	mu   sync.Mutex
	sent []Sent
}

func (s *Sender) SendVerificationCode(_ context.Context, to, fullName, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, Sent{To: to, FullName: fullName, Code: code})
	return nil
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
