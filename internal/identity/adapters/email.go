// Package adapters holds identity's outbound integrations behind
// interfaces (.claude/rules/go.md); fakes live in tools/fakes.
package adapters

import (
	"context"
	"fmt"
	"net/smtp"
)

// EmailSender sends identity's transactional emails.
type EmailSender interface {
	// SendVerificationCode sends code to to, addressed to fullName.
	SendVerificationCode(ctx context.Context, to, fullName, code string) error
	// SendPasswordResetCode sends code to to, addressed to fullName. A
	// separate method from SendVerificationCode so the two can never be
	// confused — the two emails must say different things, since one
	// invites clicking a verification flow and the other a password
	// reset.
	SendPasswordResetCode(ctx context.Context, to, fullName, code string) error
	// SendStepUpCode sends code to to, addressed to fullName — a
	// one-time code proving fresh account control for a sensitive
	// action (task 1.6), not a sign-in or password flow.
	SendStepUpCode(ctx context.Context, to, fullName, code string) error
}

// SMTPSender sends over plain SMTP — no auth, no TLS. That's exactly
// what Mailpit (local dev, deploy/compose/docker-compose.yml) needs on
// localhost:1025; a production-grade relay (auth, STARTTLS) is an
// undecided OD, not solved here.
type SMTPSender struct {
	addr string // host:port
	from string
}

// NewSMTPSender returns a sender that dials addr (host:port) for every
// message, From: from.
func NewSMTPSender(addr, from string) *SMTPSender {
	return &SMTPSender{addr: addr, from: from}
}

func (s *SMTPSender) SendVerificationCode(_ context.Context, to, fullName, code string) error {
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: Your VASPRO verification code\r\n\r\nHi %s,\r\n\r\nYour verification code is: %s\r\n\r\nIt expires in 15 minutes.\r\n",
		s.from, to, fullName, code,
	)
	if err := smtp.SendMail(s.addr, nil, s.from, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("adapters: sending verification email: %w", err)
	}
	return nil
}

func (s *SMTPSender) SendPasswordResetCode(_ context.Context, to, fullName, code string) error {
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: Your VASPRO password reset code\r\n\r\nHi %s,\r\n\r\nYour password reset code is: %s\r\n\r\nIt expires in 15 minutes. If you didn't ask to reset your password, ignore this email.\r\n",
		s.from, to, fullName, code,
	)
	if err := smtp.SendMail(s.addr, nil, s.from, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("adapters: sending password reset email: %w", err)
	}
	return nil
}

func (s *SMTPSender) SendStepUpCode(_ context.Context, to, fullName, code string) error {
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: Your VASPRO verification code\r\n\r\nHi %s,\r\n\r\nYour code to confirm this sensitive action is: %s\r\n\r\nIt expires in 15 minutes. If you didn't request this, you can ignore this email.\r\n",
		s.from, to, fullName, code,
	)
	if err := smtp.SendMail(s.addr, nil, s.from, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("adapters: sending step-up email: %w", err)
	}
	return nil
}
