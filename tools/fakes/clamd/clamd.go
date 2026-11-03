// Package clamd fakes ClamAV's clamd daemon for tests (task 2.8): a TCP
// server speaking clamd's real protocol (zPING and zINSTREAM), so
// accounts' ClamdScanner runs unchanged against it. It flags anything
// containing the EICAR test string, which every antivirus treats as a
// virus by convention. Real ClamAV needs about 1 GB of memory and a
// minute to start, so only one adapter test uses it.
package clamd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// EICAR returns the EICAR antivirus test string. It's assembled here
// rather than written out so no source file carries it whole (some
// host antivirus software quarantines files that do).
func EICAR() []byte {
	return []byte(strings.Join([]string{`X5O!P%@AP[4\PZX54(P^)7CC)7}$`, "EICAR-STANDARD-ANTIVIRUS-TEST-FILE!", "$H+H*"}, ""))
}

// Server is the fake clamd.
type Server struct {
	ln    net.Listener
	wg    sync.WaitGroup
	once  sync.Once
	scans atomic.Int64
}

// Scans is how many files the fake has scanned.
func (s *Server) Scans() int64 { return s.scans.Load() }

// Start listens on a free local port.
func Start() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is the host:port to give the scanner (ACCOUNTS_CLAMAV_ADDR).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server.
func (s *Server) Close() {
	s.once.Do(func() { _ = s.ln.Close() })
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { _ = conn.Close() }()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	r := bufio.NewReader(conn)
	cmd, err := r.ReadString(0)
	if err != nil {
		return
	}
	switch strings.TrimSuffix(cmd, "\x00") {
	case "zPING":
		_, _ = conn.Write([]byte("PONG\x00"))
	case "zINSTREAM":
		s.scans.Add(1)
		data, err := readStream(r)
		if err != nil {
			_, _ = conn.Write([]byte("INSTREAM size limit exceeded. ERROR\x00"))
			return
		}
		if bytes.Contains(data, EICAR()) {
			_, _ = conn.Write([]byte("stream: Eicar-Test-Signature FOUND\x00"))
			return
		}
		_, _ = conn.Write([]byte("stream: OK\x00"))
	default:
		_, _ = conn.Write([]byte("UNKNOWN COMMAND\x00"))
	}
}

// readStream reads INSTREAM's chunks: a 4-byte big-endian length, that
// many bytes, until a zero length.
func readStream(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		var size uint32
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return nil, err
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if out.Len()+int(size) > 25<<20 {
			return nil, errors.New("too large")
		}
		if _, err := io.CopyN(&out, r, int64(size)); err != nil {
			return nil, err
		}
	}
}
