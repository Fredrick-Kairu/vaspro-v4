// Package clamav scans files with ClamAV's clamd over its TCP protocol,
// streaming them (task 6.2a, D-23): zINSTREAM sends length-prefixed
// chunks and a zero terminator, and clamd replies "stream: OK" or
// "stream: <signature> FOUND". accounts' document scanner (task 2.8)
// has its own copy for whole files in memory.
package clamav

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ErrTooLarge means the stream was longer than clamd's StreamMaxLength,
// so clamd gave no verdict. Raising StreamMaxLength (and MaxScanSize,
// MaxFileSize) to the upload cap is a deployment setting.
var ErrTooLarge = errors.New("clamav: file is over clamd's StreamMaxLength")

// Result is a scan's verdict.
type Result struct {
	Clean     bool
	Signature string // what was found, when not clean
}

// chunkSize is how much each INSTREAM chunk carries.
const chunkSize = 64 << 10

// Client talks to one clamd. An error from it means no verdict: callers
// must treat the file as unscanned and refuse it.
type Client struct {
	addr    string
	timeout time.Duration
}

// New talks to clamd at addr (host:port). A scan without a context
// deadline is cut off after 5 minutes.
func New(addr string) *Client {
	return &Client{addr: addr, timeout: 5 * time.Minute}
}

// Ping checks clamd answers.
func (c *Client) Ping(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return fmt.Errorf("clamav: writing to clamd: %w", err)
	}
	reply, err := readReply(conn)
	if err != nil {
		return err
	}
	if reply != "PONG" {
		return fmt.Errorf("clamav: clamd answered %q to PING", reply)
	}
	return nil
}

// Scan streams r to clamd and returns its verdict.
func (c *Client) Scan(ctx context.Context, r io.Reader) (Result, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Result{}, fmt.Errorf("clamav: writing to clamd: %w", err)
	}
	if err := sendChunks(conn, r); err != nil {
		var readErr readError
		if errors.As(err, &readErr) {
			return Result{}, readErr.err
		}
		// clamd closes the connection once a stream passes its limit,
		// so a failed write may still have a reply waiting.
		if reply, rerr := readReply(conn); rerr == nil {
			return verdict(reply)
		}
		return Result{}, fmt.Errorf("clamav: streaming to clamd: %w", err)
	}
	reply, err := readReply(conn)
	if err != nil {
		return Result{}, err
	}
	return verdict(reply)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("clamav: reaching clamd: %w", err)
	}
	deadline := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clamav: setting clamd deadline: %w", err)
	}
	return conn, nil
}

// readError marks a failure reading the file, as opposed to talking to
// clamd.
type readError struct{ err error }

func (e readError) Error() string { return e.err.Error() }

func sendChunks(conn net.Conn, r io.Reader) error {
	buf := make([]byte, 4+chunkSize)
	for {
		n, err := io.ReadFull(r, buf[4:])
		if n > 0 {
			binary.BigEndian.PutUint32(buf, uint32(n)) //nolint:gosec // G115: n <= chunkSize
			if _, werr := conn.Write(buf[:4+n]); werr != nil {
				return werr
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			_, werr := conn.Write([]byte{0, 0, 0, 0})
			return werr
		default:
			return readError{fmt.Errorf("clamav: reading the file: %w", err)}
		}
	}
}

func readReply(conn net.Conn) (string, error) {
	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil {
		return "", fmt.Errorf("clamav: reading clamd's reply: %w", err)
	}
	return strings.TrimSuffix(reply, "\x00"), nil
}

func verdict(reply string) (Result, error) {
	v, ok := strings.CutPrefix(reply, "stream: ")
	switch {
	case ok && v == "OK":
		return Result{Clean: true}, nil
	case ok && strings.HasSuffix(v, " FOUND"):
		return Result{Signature: strings.TrimSuffix(v, " FOUND")}, nil
	case strings.Contains(reply, "size limit exceeded"):
		return Result{}, ErrTooLarge
	default:
		return Result{}, fmt.Errorf("clamav: clamd couldn't scan: %q", reply)
	}
}
