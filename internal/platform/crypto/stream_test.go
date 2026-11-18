package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func newTestEnvelope(t *testing.T) *Envelope {
	t.Helper()
	kms, err := NewLocalKMS(map[string][]byte{"uploads": bytes.Repeat([]byte{1}, 32), "other": bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return NewEnvelope(kms)
}

func sealAll(t *testing.T, e *Envelope, keyID string, plaintext []byte) []byte {
	t.Helper()
	s, err := e.NewStreamSealer(context.Background(), keyID)
	if err != nil {
		t.Fatalf("NewStreamSealer: %v", err)
	}
	var out bytes.Buffer
	w := s.Writer(&out)
	// Odd write sizes, so chunk boundaries never line up with writes.
	for rest := plaintext; len(rest) > 0; {
		n := min(len(rest), 7919)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := int64(out.Len()), s.SealedSize(int64(len(plaintext))); got != want {
		t.Fatalf("sealed %d bytes, SealedSize said %d", got, want)
	}
	return out.Bytes()
}

func openAll(e *Envelope, keyID string, sealed []byte) ([]byte, error) {
	r, err := e.OpenStream(context.Background(), keyID, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStream_RoundTrip(t *testing.T) {
	e := newTestEnvelope(t)
	for _, n := range []int{0, 1, StreamChunkSize - 1, StreamChunkSize, StreamChunkSize + 1, 3 * StreamChunkSize, 10 << 20} {
		plaintext := randomBytes(t, n)
		sealed := sealAll(t, e, "uploads", plaintext)
		if n >= 32 && bytes.Contains(sealed, plaintext[:32]) { // shorter runs can occur in random ciphertext by chance
			t.Fatalf("%d bytes: the sealed stream contains the plaintext", n)
		}
		got, err := openAll(e, "uploads", sealed)
		if err != nil {
			t.Fatalf("%d bytes: OpenStream: %v", n, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("%d bytes: round trip changed the data", n)
		}
	}
}

func TestStream_TwoSealsDiffer(t *testing.T) {
	e := newTestEnvelope(t)
	plaintext := []byte("name,city\nTest,Nairobi\n")
	if bytes.Equal(sealAll(t, e, "uploads", plaintext), sealAll(t, e, "uploads", plaintext)) {
		t.Fatal("two seals of the same file are identical: data key or nonce reused")
	}
}

func TestStream_RefusesTampering(t *testing.T) {
	e := newTestEnvelope(t)
	plaintext := randomBytes(t, 2*StreamChunkSize+100) // three chunks
	sealed := sealAll(t, e, "uploads", plaintext)
	header := len(sealed) - (len(plaintext) + 3*streamTagSize)
	chunk := StreamChunkSize + streamTagSize

	mustFail := func(name string, blob []byte, keyID string) {
		t.Helper()
		if _, err := openAll(e, keyID, blob); err == nil {
			t.Errorf("%s: opened without error", name)
		}
	}

	// Every byte of the header, and a sample of bytes in every chunk.
	for i := range header {
		b := bytes.Clone(sealed)
		b[i] ^= 0x01
		mustFail("header byte flipped", b, "uploads")
	}
	for i := header; i < len(sealed); i += 997 {
		b := bytes.Clone(sealed)
		b[i] ^= 0x80
		mustFail("chunk byte flipped", b, "uploads")
	}

	mustFail("truncated at the first chunk boundary", sealed[:header+chunk], "uploads")
	mustFail("truncated at the second chunk boundary", sealed[:header+2*chunk], "uploads")
	mustFail("truncated inside a chunk", sealed[:len(sealed)-1], "uploads")
	mustFail("header only", sealed[:header], "uploads")
	mustFail("empty", nil, "uploads")
	mustFail("extra bytes after the final chunk", append(bytes.Clone(sealed), 0), "uploads")

	swapped := bytes.Clone(sealed[:header])
	swapped = append(swapped, sealed[header+chunk:header+2*chunk]...)
	swapped = append(swapped, sealed[header:header+chunk]...)
	swapped = append(swapped, sealed[header+2*chunk:]...)
	mustFail("first two chunks swapped", swapped, "uploads")

	dropped := append(bytes.Clone(sealed[:header+chunk]), sealed[header+2*chunk:]...)
	mustFail("second chunk dropped", dropped, "uploads")

	mustFail("wrong KMS key", sealed, "other")

	// A chunk lifted from another file sealed under the same key.
	other := sealAll(t, e, "uploads", randomBytes(t, len(plaintext)))
	spliced := append(bytes.Clone(sealed[:header+chunk]), other[header+chunk:]...)
	mustFail("chunks from another file", spliced, "uploads")
}

func TestStream_WriteAfterCloseFails(t *testing.T) {
	e := newTestEnvelope(t)
	s, err := e.NewStreamSealer(context.Background(), "uploads")
	if err != nil {
		t.Fatal(err)
	}
	w := s.Writer(io.Discard)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Fatal("a write after Close succeeded")
	}
}

func TestStream_UnknownKMSKey(t *testing.T) {
	e := newTestEnvelope(t)
	if _, err := e.NewStreamSealer(context.Background(), "missing"); err == nil {
		t.Fatal("sealing under an unknown KMS key succeeded")
	}
}

// failingWriter fails after n bytes, to check errors from the
// destination reach the caller.
type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.n {
		return 0, errors.New("disk full")
	}
	f.n -= len(p)
	return len(p), nil
}

func TestStream_DestinationErrorsSurface(t *testing.T) {
	e := newTestEnvelope(t)
	s, err := e.NewStreamSealer(context.Background(), "uploads")
	if err != nil {
		t.Fatal(err)
	}
	w := s.Writer(&failingWriter{n: 200})
	_, werr := w.Write(make([]byte, 3*StreamChunkSize))
	cerr := w.Close()
	if werr == nil && cerr == nil {
		t.Fatal("a failing destination went unreported")
	}
}

// BenchmarkStream_Seal100MB shows sealing allocates a fixed amount,
// whatever the size: run with -benchmem and compare B/op to the 100 MB
// written.
func BenchmarkStream_Seal100MB(b *testing.B) {
	kms, _ := NewLocalKMS(map[string][]byte{"uploads": bytes.Repeat([]byte{1}, 32)})
	e := NewEnvelope(kms)
	chunk := make([]byte, 1<<20)
	b.ReportAllocs()
	b.SetBytes(100 << 20)
	for b.Loop() {
		s, err := e.NewStreamSealer(context.Background(), "uploads")
		if err != nil {
			b.Fatal(err)
		}
		w := s.Writer(io.Discard)
		for range 100 {
			if _, err := w.Write(chunk); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestStream_SealingAllocatesAFixedAmount(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 100 MB of output")
	}
	res := testing.Benchmark(BenchmarkStream_Seal100MB)
	// 100 MB passes through; the sealer itself should hold one chunk
	// and its ciphertext, not the file.
	if perOp := res.AllocedBytesPerOp(); perOp > 1<<20 {
		t.Fatalf("sealing 100 MB allocated %d bytes; want under 1 MB", perOp)
	}
}
