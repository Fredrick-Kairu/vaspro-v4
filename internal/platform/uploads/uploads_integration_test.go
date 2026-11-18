//go:build integration

package uploads_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

type realHarness struct {
	up    *uploads.Uploads
	store *objectstore.Store
	clamd *clamd.Server
}

func newRealHarness(t *testing.T, maxBytes int64) *realHarness {
	t.Helper()
	cfg := testkit.StartObjectStorage(t)
	cfg.Bucket = "uploads"
	cfg.PublicEndpoint = cfg.Endpoint
	store, err := objectstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	// clamd's StreamMaxLength must be at least the cap; the default cap
	// is under clamd's default, and a larger one needs both raised.
	if maxBytes > clamd.DefaultStreamMaxLength {
		fake.SetStreamMaxLength(maxBytes)
	}
	kms, err := crypto.NewLocalKMS(map[string][]byte{"uploads": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	upCfg := uploads.DefaultConfig()
	upCfg.KMSKeyID = "uploads"
	upCfg.DownloadKey = bytes.Repeat([]byte{7}, 32)
	if maxBytes > 0 {
		upCfg.MaxBytes = maxBytes
	}
	// Storage checks the form's expiry against real time.
	up, err := uploads.New(store, clamav.New(fake.Addr()), crypto.NewEnvelope(kms), clock.Real{}, upCfg)
	if err != nil {
		t.Fatal(err)
	}
	return &realHarness{up: up, store: store, clamd: fake}
}

// postStreaming uploads size bytes from body through the signed form,
// as a browser would, without holding the file.
func postStreaming(t *testing.T, form objectstore.UploadForm, body io.Reader) int {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		for k, v := range form.Fields {
			if err := mw.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		fw, err := mw.CreateFormFile("file", "contacts.csv")
		if err == nil {
			_, err = io.Copy(fw, body)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := http.NewRequest(http.MethodPost, form.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting the form: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestUploads_ThroughSignedForm(t *testing.T) {
	ctx := context.Background()
	h := newRealHarness(t, 0)
	tenant, id := uuid.New(), uuid.New()

	up, err := h.up.IssueUpload(ctx, tenant, id)
	if err != nil {
		t.Fatalf("IssueUpload: %v", err)
	}
	body := csvBody(2000)
	if code := postStreaming(t, up.Form, bytes.NewReader(body)); code/100 != 2 {
		t.Fatalf("upload answered %d", code)
	}
	file, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if sum := sha256.Sum256(body); !bytes.Equal(file.SHA256, sum[:]) {
		t.Fatal("wrong SHA-256")
	}
	if _, err := h.store.Stat(ctx, uploads.QuarantineKey(tenant, id)); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("quarantine copy left: %v", err)
	}
	stored, err := h.store.Get(ctx, uploads.FileKey(uploads.Transient, tenant, id))
	if err != nil || bytes.Contains(stored, []byte("Test Person")) {
		t.Fatalf("stored object missing or in plaintext (%v)", err)
	}
	r, err := h.up.Open(ctx, uploads.FileKey(uploads.Transient, tenant, id))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}

	t.Run("an infected upload leaves nothing in storage", func(t *testing.T) {
		id := uuid.New()
		up, err := h.up.IssueUpload(ctx, tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		if code := postStreaming(t, up.Form, bytes.NewReader(append(csvBody(10), clamd.EICAR()...))); code/100 != 2 {
			t.Fatalf("upload answered %d", code)
		}
		if _, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV); !errors.Is(err, uploads.ErrInfected) {
			t.Fatalf("got %v", err)
		}
		for _, k := range []string{uploads.QuarantineKey(tenant, id), uploads.FileKey(uploads.Transient, tenant, id)} {
			if _, err := h.store.Stat(ctx, k); !errors.Is(err, objectstore.ErrNotFound) {
				t.Errorf("%s exists: %v", k, err)
			}
		}
	})
}

// TestUploads_AtTheDefaultCap finalizes a file of exactly the default
// cap with clamd's default limit: nothing in ClamAV needs changing.
func TestUploads_AtTheDefaultCap(t *testing.T) {
	ctx := context.Background()
	h := newRealHarness(t, 0)
	tenant, id := uuid.New(), uuid.New()
	up, err := h.up.IssueUpload(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte("Test Person,Nairobi,2026-11-16\n")
	if code := postStreaming(t, up.Form, io.LimitReader(&repeatReader{line: line}, up.MaxBytes)); code/100 != 2 {
		t.Fatalf("upload answered %d", code)
	}
	if _, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV); err != nil {
		t.Fatalf("Finalize at the cap: %v", err)
	}

	// One byte more is refused by storage, before anything is scanned.
	id = uuid.New()
	up, err = h.up.IssueUpload(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if code := postStreaming(t, up.Form, io.LimitReader(&repeatReader{line: line}, up.MaxBytes+1)); code/100 == 2 {
		t.Fatalf("one byte over the cap answered %d", code)
	}
	if _, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV); !errors.Is(err, uploads.ErrNotUploaded) {
		t.Fatalf("after a refused upload: %v", err)
	}
}

// TestUploads_100MBWithFlatMemory finalizes 100 MB, five times the
// default cap, and samples the heap throughout: neither read may hold
// the file, so memory doesn't depend on the cap.
func TestUploads_100MBWithFlatMemory(t *testing.T) {
	ctx := context.Background()
	h := newRealHarness(t, 100<<20)
	tenant, id := uuid.New(), uuid.New()
	up, err := h.up.IssueUpload(ctx, tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	const size = 100 << 20
	line := []byte("Test Person,Nairobi,2026-11-16,some attribute value here\n")
	if code := postStreaming(t, up.Form, io.LimitReader(&repeatReader{line: line}, size)); code/100 != 2 {
		t.Fatalf("upload answered %d", code)
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	began := time.Now()
	file, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV)
	took := time.Since(began)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if file.Size != size {
		t.Fatalf("size %d", file.Size)
	}
	growth := int64(peak.Load()) - int64(base.HeapAlloc) //nolint:gosec // G115: heap sizes
	t.Logf("finalized 100 MB in %s; peak heap growth %.1f MB", took.Round(time.Millisecond), float64(growth)/(1<<20))
	if growth > 64<<20 {
		t.Fatalf("peak heap grew %d MB while finalizing 100 MB; want under 64 MB", growth>>20)
	}

	// The stored file opens back to the same bytes, streamed.
	r, err := h.up.Open(ctx, uploads.FileKey(uploads.Transient, tenant, id))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, r)
	_ = r.Close()
	if err != nil || n != size || !bytes.Equal(hash.Sum(nil), file.SHA256) {
		t.Fatalf("read back %d bytes, %v", n, err)
	}
}

type repeatReader struct {
	line []byte
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.line[r.off:])
		n += c
		r.off = (r.off + c) % len(r.line)
	}
	return n, nil
}
