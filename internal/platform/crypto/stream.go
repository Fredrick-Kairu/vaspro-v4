package crypto

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Streaming envelope encryption (task 6.2a, D-23): Envelope's scheme
// for files too large to hold in memory, such as 100 MB contact
// imports. A fresh data key is wrapped by the KMS as Seal does, then
// the file is sealed in StreamChunkSize chunks with AES-256-GCM.
//
// Format: "VSS1" | uint16 length of the wrapped key | wrapped key |
// 7-byte random nonce prefix, then each chunk's ciphertext and tag.
// Chunk i's nonce is prefix | uint32 i | 1 for the last chunk, else 0,
// and its additional data is the whole header. So a chunk can't be
// moved, dropped, taken from another file or have the stream cut after
// it, and the header can't be changed: each fails to open. Every chunk
// but the last is full; the last holds 0 to StreamChunkSize bytes.

// StreamChunkSize is the plaintext size of every chunk but the last.
const StreamChunkSize = 64 << 10

const (
	streamTagSize    = 16
	streamPrefixSize = 7
	streamMaxChunks  = 1 << 32
)

var streamMagic = []byte("VSS1")

// ErrStreamCorrupt means a sealed stream was changed, cut short, or
// sealed under another key.
var ErrStreamCorrupt = errors.New("crypto: sealed stream is corrupt or under another key")

// StreamSealer seals one file. It holds the file's data key, so use it
// for one stream and drop it.
type StreamSealer struct {
	aead   cipher.AEAD
	header []byte
	prefix []byte
}

// NewStreamSealer makes a data key for one file and wraps it under
// keyID.
func (e *Envelope) NewStreamSealer(ctx context.Context, keyID string) (*StreamSealer, error) {
	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: generating data key: %w", err)
	}
	defer zero(dek)
	encDEK, err := e.kms.Encrypt(ctx, keyID, dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypting data key: %w", err)
	}
	if len(encDEK) > 0xFFFF {
		return nil, errors.New("crypto: wrapped data key too long for a stream header")
	}
	prefix := make([]byte, streamPrefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return nil, fmt.Errorf("crypto: generating nonce prefix: %w", err)
	}
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, len(streamMagic)+2+len(encDEK)+streamPrefixSize)
	header = append(header, streamMagic...)
	header = binary.BigEndian.AppendUint16(header, uint16(len(encDEK))) //nolint:gosec // G115: checked above
	header = append(header, encDEK...)
	header = append(header, prefix...)
	return &StreamSealer{aead: aead, header: header, prefix: prefix}, nil
}

// SealedSize is exactly how many bytes Writer produces for n bytes of
// plaintext, so an upload can declare its length before it starts.
func (s *StreamSealer) SealedSize(n int64) int64 {
	chunks := max(1, (n+StreamChunkSize-1)/StreamChunkSize)
	return int64(len(s.header)) + n + chunks*streamTagSize
}

// Writer returns a writer that seals what's written to it into dst.
// Close writes the last chunk; without it the stream won't open.
func (s *StreamSealer) Writer(dst io.Writer) io.WriteCloser {
	return &streamWriter{s: s, dst: dst, buf: make([]byte, 0, StreamChunkSize),
		out: make([]byte, 0, StreamChunkSize+streamTagSize)}
}

type streamWriter struct {
	s       *StreamSealer
	dst     io.Writer
	buf     []byte // plaintext not yet sealed
	out     []byte
	counter uint64
	started bool // header written
	closed  bool
	err     error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("crypto: write to a closed stream")
	}
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		// A full buffer is sealed only once more data arrives, so the
		// last chunk is always the one Close seals.
		if len(w.buf) == StreamChunkSize {
			if err := w.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(w.buf[len(w.buf):StreamChunkSize], p)
		w.buf = w.buf[:len(w.buf)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

func (w *streamWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	return w.flush(true)
}

func (w *streamWriter) flush(last bool) error {
	if !w.started {
		if _, err := w.dst.Write(w.s.header); err != nil {
			w.err = fmt.Errorf("crypto: writing stream header: %w", err)
			return w.err
		}
		w.started = true
	}
	if w.counter >= streamMaxChunks {
		w.err = errors.New("crypto: stream too long")
		return w.err
	}
	nonce := streamNonce(w.s.prefix, w.counter, last)
	w.out = w.s.aead.Seal(w.out[:0], nonce, w.buf, w.s.header)
	if _, err := w.dst.Write(w.out); err != nil {
		w.err = fmt.Errorf("crypto: writing sealed chunk: %w", err)
		return w.err
	}
	w.counter++
	w.buf = w.buf[:0]
	return nil
}

func streamNonce(prefix []byte, counter uint64, last bool) []byte {
	nonce := make([]byte, 0, streamPrefixSize+5)
	nonce = append(nonce, prefix...)
	nonce = binary.BigEndian.AppendUint32(nonce, uint32(counter)) //nolint:gosec // G115: counter < streamMaxChunks
	if last {
		return append(nonce, 1)
	}
	return append(nonce, 0)
}

// OpenStream reads src's header, unwraps its data key under keyID and
// returns a reader of the plaintext. Every chunk is checked before its
// bytes are returned; a changed, reordered or cut-short stream ends in
// ErrStreamCorrupt, so a reader must treat any error as "discard what
// was read".
func (e *Envelope) OpenStream(ctx context.Context, keyID string, src io.Reader) (io.Reader, error) {
	fixed := make([]byte, len(streamMagic)+2)
	if _, err := io.ReadFull(src, fixed); err != nil {
		return nil, ErrStreamCorrupt
	}
	if !bytes.Equal(fixed[:len(streamMagic)], streamMagic) {
		return nil, ErrStreamCorrupt
	}
	rest := make([]byte, int(binary.BigEndian.Uint16(fixed[len(streamMagic):]))+streamPrefixSize)
	if _, err := io.ReadFull(src, rest); err != nil {
		return nil, ErrStreamCorrupt
	}
	encDEK, prefix := rest[:len(rest)-streamPrefixSize], rest[len(rest)-streamPrefixSize:]
	dek, err := e.kms.Decrypt(ctx, keyID, encDEK)
	if err != nil {
		return nil, ErrStreamCorrupt
	}
	defer zero(dek)
	aead, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	return &streamReader{
		aead: aead, header: append(fixed, rest...), prefix: prefix,
		src: src, in: make([]byte, StreamChunkSize+streamTagSize+1),
	}, nil
}

type streamReader struct {
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	src     io.Reader
	in      []byte // one sealed chunk plus one byte of lookahead
	held    int    // bytes of in already read from src
	plain   []byte // opened, not yet returned
	out     []byte // reused for each opened chunk
	counter uint64
	done    bool
	err     error
}

func (r *streamReader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		r.next()
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

// next opens one chunk. It reads a full sealed chunk plus one byte: if
// that byte exists another chunk follows, otherwise this is the last.
func (r *streamReader) next() {
	full := StreamChunkSize + streamTagSize
	n, err := io.ReadFull(r.src, r.in[r.held:])
	r.held += n
	switch {
	case err == nil: // a full chunk and one more byte
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
	default:
		r.err = fmt.Errorf("crypto: reading sealed stream: %w", err)
		return
	}
	last := r.held <= full
	size := min(r.held, full)
	if size < streamTagSize || r.counter >= streamMaxChunks {
		r.err = ErrStreamCorrupt
		return
	}
	plain, openErr := r.aead.Open(r.out[:0], streamNonce(r.prefix, r.counter, last), r.in[:size], r.header)
	if openErr != nil {
		r.err = ErrStreamCorrupt
		return
	}
	r.counter++
	r.out = plain
	r.plain = plain
	if last {
		r.done = true
		return
	}
	// Keep the lookahead byte as the start of the next chunk.
	r.in[0] = r.in[full]
	r.held = 1
}
