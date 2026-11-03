package adapters

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// ScanResult is a virus scan's verdict.
type ScanResult struct {
	Clean     bool
	Signature string // what was found, when not clean
}

// Scanner scans an upload before it's stored (task 2.8). An error means
// no verdict: callers refuse the upload rather than store it unscanned.
type Scanner interface {
	Scan(ctx context.Context, data []byte) (ScanResult, error)
}

// scanChunk is how much INSTREAM sends per chunk.
const scanChunk = 1 << 20

// ClamdScanner is Scanner over ClamAV's clamd TCP protocol (zINSTREAM:
// length-prefixed chunks and a zero terminator; the reply is "stream:
// OK" or "stream: <signature> FOUND").
type ClamdScanner struct {
	addr    string
	timeout time.Duration
}

// NewClamdScanner talks to clamd at addr (host:port).
func NewClamdScanner(addr string) *ClamdScanner {
	return &ClamdScanner{addr: addr, timeout: 60 * time.Second}
}

// Ping checks clamd answers.
func (c *ClamdScanner) Ping(ctx context.Context) error {
	reply, err := c.exchange(ctx, "zPING\x00", nil)
	if err != nil {
		return err
	}
	if reply != "PONG" {
		return fmt.Errorf("adapters: clamd answered %q to PING", reply)
	}
	return nil
}

// Scan streams data to clamd and reads its verdict.
func (c *ClamdScanner) Scan(ctx context.Context, data []byte) (ScanResult, error) {
	reply, err := c.exchange(ctx, "zINSTREAM\x00", data)
	if err != nil {
		return ScanResult{}, err
	}
	verdict, ok := strings.CutPrefix(reply, "stream: ")
	switch {
	case ok && verdict == "OK":
		return ScanResult{Clean: true}, nil
	case ok && strings.HasSuffix(verdict, " FOUND"):
		return ScanResult{Signature: strings.TrimSuffix(verdict, " FOUND")}, nil
	default:
		return ScanResult{}, fmt.Errorf("adapters: clamd couldn't scan: %q", reply)
	}
}

// exchange sends one command (with an INSTREAM body when data isn't
// nil) and returns clamd's NUL-terminated reply.
func (c *ClamdScanner) exchange(ctx context.Context, command string, data []byte) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return "", fmt.Errorf("adapters: reaching clamd: %w", err)
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("adapters: setting clamd deadline: %w", err)
	}

	if _, err := conn.Write([]byte(command)); err != nil {
		return "", fmt.Errorf("adapters: writing to clamd: %w", err)
	}
	if data != nil {
		for start := 0; start < len(data); start += scanChunk {
			end := min(start+scanChunk, len(data))
			if err := binary.Write(conn, binary.BigEndian, uint32(end-start)); err != nil { //nolint:gosec // G115: a chunk is at most scanChunk (1 MB)
				return "", fmt.Errorf("adapters: streaming to clamd: %w", err)
			}
			if _, err := conn.Write(data[start:end]); err != nil {
				return "", fmt.Errorf("adapters: streaming to clamd: %w", err)
			}
		}
		if err := binary.Write(conn, binary.BigEndian, uint32(0)); err != nil {
			return "", fmt.Errorf("adapters: ending the clamd stream: %w", err)
		}
	}
	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil {
		return "", fmt.Errorf("adapters: reading clamd's reply: %w", err)
	}
	return strings.TrimSuffix(reply, "\x00"), nil
}
