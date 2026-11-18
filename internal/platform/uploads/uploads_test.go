package uploads_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
	fakestore "github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/objectstore"
)

var start = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

type harness struct {
	up    *uploads.Uploads
	store *fakestore.Store
	clamd *clamd.Server
	clock *clock.Fake
	env   *crypto.Envelope
}

func testConfig() uploads.Config {
	cfg := uploads.DefaultConfig()
	cfg.KMSKeyID = "uploads"
	cfg.DownloadKey = bytes.Repeat([]byte{7}, 32)
	cfg.MaxBytes = 4 << 20
	return cfg
}

func newHarness(t *testing.T, mutate ...func(*uploads.Config)) *harness {
	t.Helper()
	kms, err := crypto.NewLocalKMS(map[string][]byte{"uploads": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	fake, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	h := &harness{store: fakestore.New(), clamd: fake, clock: clock.NewFake(start), env: crypto.NewEnvelope(kms)}
	cfg := testConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	h.up, err = uploads.New(h.store, clamav.New(fake.Addr()), h.env, h.clock, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// upload puts body where the signed form would have put it.
func (h *harness) upload(t *testing.T, tenant, id uuid.UUID, body []byte) {
	t.Helper()
	if err := h.store.Put(context.Background(), uploads.QuarantineKey(tenant, id), body); err != nil {
		t.Fatal(err)
	}
}

func csvBody(rows int) []byte {
	var b bytes.Buffer
	b.WriteString("name,city,joined\n")
	for i := range rows {
		b.WriteString("Test Person,Nairobi,2026-11-")
		b.WriteString(string(rune('1' + i%9)))
		b.WriteString("\n")
	}
	return b.Bytes()
}

func xlsxBody(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/worksheets/sheet1.xml"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("<x/>"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestConfig_Defaults(t *testing.T) {
	cfg := uploads.DefaultConfig()
	if cfg.MaxBytes != 20<<20 || cfg.UploadLinkTTL != 15*time.Minute || cfg.DownloadLinkTTL != 5*time.Minute ||
		cfg.TransientRetention != 48*time.Hour || cfg.FinancialRetention != 30*24*time.Hour {
		t.Fatalf("defaults are not data model §8's: %+v", cfg)
	}
	if cfg.MaxBytes >= clamd.DefaultStreamMaxLength {
		t.Fatalf("the default cap %d isn't under clamd's default StreamMaxLength %d", cfg.MaxBytes, clamd.DefaultStreamMaxLength)
	}
}

func TestNew_RefusesUnusableSettings(t *testing.T) {
	kms, _ := crypto.NewLocalKMS(map[string][]byte{"uploads": bytes.Repeat([]byte{3}, 32)})
	env := crypto.NewEnvelope(kms)
	for name, mutate := range map[string]func(*uploads.Config){
		"no KMS key":                func(c *uploads.Config) { c.KMSKeyID = "" },
		"a short download key":      func(c *uploads.Config) { c.DownloadKey = []byte("short") },
		"no size cap":               func(c *uploads.Config) { c.MaxBytes = 0 },
		"no upload link lifetime":   func(c *uploads.Config) { c.UploadLinkTTL = 0 },
		"no download lifetime":      func(c *uploads.Config) { c.DownloadLinkTTL = 0 },
		"no transient retention":    func(c *uploads.Config) { c.TransientRetention = 0 },
		"financial under transient": func(c *uploads.Config) { c.FinancialRetention = time.Hour },
	} {
		cfg := testConfig()
		mutate(&cfg)
		if _, err := uploads.New(fakestore.New(), clamav.New("127.0.0.1:1"), env, clock.NewFake(start), cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestKeys_BuiltOnlyFromIDs(t *testing.T) {
	pattern := regexp.MustCompile(`^(quarantine/[0-9a-f-]{36}/[0-9a-f-]{36}|uploads/(transient|financial)/[0-9a-f-]{36}/[0-9a-f-]{36}(\.errors)?)$`)
	for range 1000 {
		tenant, id := uuid.New(), uuid.New()
		for _, k := range []string{
			uploads.QuarantineKey(tenant, id),
			uploads.FileKey(uploads.Transient, tenant, id),
			uploads.FileKey(uploads.Financial, tenant, id),
			uploads.ErrorReportKey(uploads.Transient, tenant, id),
		} {
			if !pattern.MatchString(k) || !strings.Contains(k, tenant.String()+"/"+id.String()) {
				t.Fatalf("key %q isn't <prefix>/<tenant>/<id>", k)
			}
		}
	}
	tenant, id := uuid.New(), uuid.New()
	if uploads.QuarantineKey(tenant, id) == uploads.FileKey(uploads.Transient, tenant, id) ||
		uploads.FileKey(uploads.Transient, tenant, id) == uploads.FileKey(uploads.Financial, tenant, id) {
		t.Fatal("quarantine and the classes share a key")
	}
}

func TestDeleteAfter_PerClass(t *testing.T) {
	h := newHarness(t)
	done := start.Add(3 * time.Hour)
	if got := h.up.DeleteAfter(uploads.Transient, done); !got.Equal(done.Add(48 * time.Hour)) {
		t.Errorf("transient: %v", got)
	}
	if got := h.up.DeleteAfter(uploads.Financial, done); !got.Equal(done.Add(30 * 24 * time.Hour)) {
		t.Errorf("financial: %v", got)
	}
	short := newHarness(t, func(c *uploads.Config) { c.TransientRetention = time.Hour })
	if got := short.up.DeleteAfter(uploads.Transient, done); !got.Equal(done.Add(time.Hour)) {
		t.Errorf("configured transient retention ignored: %v", got)
	}
}

func TestIssueUpload_SignsTheQuarantineKey(t *testing.T) {
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	up, err := h.up.IssueUpload(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("IssueUpload: %v", err)
	}
	if up.Form.Fields["key"] != uploads.QuarantineKey(tenant, id) {
		t.Errorf("signed for %q", up.Form.Fields["key"])
	}
	if up.Form.Fields["x-limit"] != "4194304" || up.MaxBytes != 4<<20 {
		t.Errorf("limit %q / %d, want the configured cap", up.Form.Fields["x-limit"], up.MaxBytes)
	}
	if !up.ExpiresAt.Equal(start.Add(15*time.Minute)) || up.Form.Fields["x-until"] != start.Add(15*time.Minute).Format(time.RFC3339) {
		t.Errorf("expires %v / %q, want 15 minutes", up.ExpiresAt, up.Form.Fields["x-until"])
	}
	// An upload that never happens still goes: the link's end plus 2 days.
	if !up.DeleteAfter.Equal(start.Add(15*time.Minute + 48*time.Hour)) {
		t.Errorf("delete_after %v", up.DeleteAfter)
	}
}

func TestFinalize_StoresTheFileSealed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	body := csvBody(5000)
	h.upload(t, tenant, id, body)

	file, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	sum := sha256.Sum256(body)
	if file.Size != int64(len(body)) || !bytes.Equal(file.SHA256, sum[:]) {
		t.Fatalf("file %d bytes, sha %x; want %d, %x", file.Size, file.SHA256, len(body), sum)
	}
	if _, ok := h.store.Object(uploads.QuarantineKey(tenant, id)); ok {
		t.Error("the quarantine copy is still there")
	}
	stored, ok := h.store.Object(uploads.FileKey(uploads.Transient, tenant, id))
	if !ok {
		t.Fatal("no final object")
	}
	if bytes.Contains(stored, []byte("Test Person")) || bytes.Contains(stored, []byte("name,city")) {
		t.Fatal("the stored object contains plaintext")
	}
	r, err := h.up.Open(ctx, uploads.FileKey(uploads.Transient, tenant, id))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("Open returned %d bytes, %v", len(got), err)
	}
	if h.clamd.Scans() != 1 {
		t.Errorf("%d scans, want 1", h.clamd.Scans())
	}

	// Again, with nothing left in quarantine: safe, and says so.
	if _, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV); !errors.Is(err, uploads.ErrAlreadyFinalized) {
		t.Fatalf("a second Finalize: %v", err)
	}
}

func TestFinalize_AcceptsXLSX(t *testing.T) {
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	h.upload(t, tenant, id, xlsxBody(t))
	if _, err := h.up.Finalize(context.Background(), uploads.Transient, tenant, id, uploads.XLSX); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func TestFinalize_Refusals(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 100)...)
	pdf := []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\x00<<>>")
	cases := []struct {
		name string
		want uploads.ContentType
		body []byte
		err  error
	}{
		{"a PNG sent as CSV", uploads.CSV, png, uploads.ErrWrongType},
		{"a PDF sent as CSV", uploads.CSV, pdf, uploads.ErrWrongType},
		{"text with NUL bytes sent as CSV", uploads.CSV, []byte("name\x00city\n"), uploads.ErrWrongType},
		{"a CSV sent as XLSX", uploads.XLSX, csvBody(3), uploads.ErrWrongType},
		{"a PNG sent as XLSX", uploads.XLSX, png, uploads.ErrWrongType},
		{"an unknown type", uploads.ContentType("application/pdf"), pdf, uploads.ErrWrongType},
		{"over the size cap", uploads.CSV, bytes.Repeat([]byte("a,b\n"), (4<<20)/4+1), uploads.ErrTooLarge},
		{"empty", uploads.CSV, []byte{}, uploads.ErrEmpty},
		{"infected", uploads.CSV, append(csvBody(10), clamd.EICAR()...), uploads.ErrInfected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			tenant, id := uuid.New(), uuid.New()
			h.upload(t, tenant, id, c.body)
			_, err := h.up.Finalize(context.Background(), uploads.Transient, tenant, id, c.want)
			if !errors.Is(err, c.err) {
				t.Fatalf("got %v, want %v", err, c.err)
			}
			// Refused files are deleted at once, and nothing is kept.
			if keys := h.store.Keys(); len(keys) != 0 {
				t.Fatalf("left behind: %v", keys)
			}
		})
	}
}

func TestFinalize_NothingUploaded(t *testing.T) {
	h := newHarness(t)
	if _, err := h.up.Finalize(context.Background(), uploads.Transient, uuid.New(), uuid.New(), uploads.CSV); !errors.Is(err, uploads.ErrNotUploaded) {
		t.Fatalf("got %v", err)
	}
}

func TestFinalize_ScannerDownKeepsTheQuarantineCopy(t *testing.T) {
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	h.upload(t, tenant, id, csvBody(10))
	h.clamd.Close()
	_, err := h.up.Finalize(context.Background(), uploads.Transient, tenant, id, uploads.CSV)
	if !errors.Is(err, uploads.ErrScanUnavailable) {
		t.Fatalf("got %v, want ErrScanUnavailable", err)
	}
	if keys := h.store.Keys(); len(keys) != 1 || keys[0] != uploads.QuarantineKey(tenant, id) {
		t.Fatalf("want only the quarantine copy, for a retry; have %v", keys)
	}
}

func TestFinalize_ScannerLimitIsTooLarge(t *testing.T) {
	h := newHarness(t)
	h.clamd.SetStreamMaxLength(1 << 10)
	tenant, id := uuid.New(), uuid.New()
	h.upload(t, tenant, id, csvBody(500))
	if _, err := h.up.Finalize(context.Background(), uploads.Transient, tenant, id, uploads.CSV); !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if keys := h.store.Keys(); len(keys) != 0 {
		t.Fatalf("left behind: %v", keys)
	}
}

func TestFinalize_FileSwappedBetweenReadsIsRefused(t *testing.T) {
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	q := uploads.QuarantineKey(tenant, id)
	body := csvBody(100)
	h.upload(t, tenant, id, body)
	// After the scan's read, the holder of the link uploads again (same
	// length, other bytes): the encryption pass would read a file
	// nobody scanned.
	h.store.OnGet(func(key string, count int) {
		if key == q && count == 1 {
			_ = h.store.Put(context.Background(), q, bytes.Replace(body, []byte("Nairobi"), []byte("Mombasa"), 1))
		}
	})
	_, err := h.up.Finalize(context.Background(), uploads.Transient, tenant, id, uploads.CSV)
	if !errors.Is(err, uploads.ErrChanged) {
		t.Fatalf("got %v, want ErrChanged", err)
	}
	if keys := h.store.Keys(); len(keys) != 0 {
		t.Fatalf("left behind: %v", keys)
	}
}

func TestOpen_DeletedFile(t *testing.T) {
	h := newHarness(t)
	if _, err := h.up.Open(context.Background(), uploads.FileKey(uploads.Transient, uuid.New(), uuid.New())); !errors.Is(err, uploads.ErrDeleted) {
		t.Fatalf("got %v, want ErrDeleted", err)
	}
}

func TestOpen_TamperedFileFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant, id := uuid.New(), uuid.New()
	h.upload(t, tenant, id, csvBody(100))
	if _, err := h.up.Finalize(ctx, uploads.Transient, tenant, id, uploads.CSV); err != nil {
		t.Fatal(err)
	}
	key := uploads.FileKey(uploads.Transient, tenant, id)
	stored, _ := h.store.Object(key)
	stored[len(stored)-5] ^= 1
	_ = h.store.Put(ctx, key, stored)
	r, err := h.up.Open(ctx, key)
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("a tampered file opened")
	}
}

func TestPutSealed_ErrorReportRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	key := uploads.ErrorReportKey(uploads.Transient, uuid.New(), uuid.New())
	report := []byte("row,field,error\n3,phone,VAL-4000 not a mobile number\n")
	if err := h.up.PutSealed(ctx, key, bytes.NewReader(report)); err != nil {
		t.Fatalf("PutSealed: %v", err)
	}
	stored, _ := h.store.Object(key)
	if bytes.Contains(stored, []byte("not a mobile number")) {
		t.Fatal("the report is stored in plaintext")
	}
	r, err := h.up.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, report) {
		t.Fatalf("got %q", got)
	}
}

func TestDownloadLinks(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	key := uploads.FileKey(uploads.Transient, tenant, uuid.New())
	token, expires, err := h.up.SignDownload(tenant, key)
	if err != nil {
		t.Fatalf("SignDownload: %v", err)
	}
	if !expires.Equal(start.Add(5 * time.Minute)) {
		t.Errorf("expires %v, want 5 minutes", expires)
	}
	got, err := h.up.VerifyDownload(token)
	if err != nil || got.Tenant != tenant || got.Key != key {
		t.Fatalf("VerifyDownload: %+v, %v", got, err)
	}

	t.Run("any changed character is refused", func(t *testing.T) {
		for i := range token {
			b := []byte(token)
			if b[i] == 'A' {
				b[i] = 'B'
			} else {
				b[i] = 'A'
			}
			if _, err := h.up.VerifyDownload(string(b)); err == nil {
				t.Fatalf("changing character %d of %q passed", i, token)
			}
		}
	})
	t.Run("garbage is refused", func(t *testing.T) {
		for _, bad := range []string{"", ".", "abc", "abc.def", token + ".x", strings.Split(token, ".")[0]} {
			if _, err := h.up.VerifyDownload(bad); !errors.Is(err, uploads.ErrLinkInvalid) {
				t.Errorf("%q: %v", bad, err)
			}
		}
	})
	t.Run("another signing key's link is refused", func(t *testing.T) {
		other := newHarness(t, func(c *uploads.Config) { c.DownloadKey = bytes.Repeat([]byte{8}, 32) })
		if _, err := other.up.VerifyDownload(token); !errors.Is(err, uploads.ErrLinkInvalid) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("expired after 5 minutes, not before", func(t *testing.T) {
		h.clock.Set(start.Add(5 * time.Minute))
		if _, err := h.up.VerifyDownload(token); err != nil {
			t.Fatalf("at exactly 5 minutes: %v", err)
		}
		h.clock.Set(start.Add(5*time.Minute + time.Second))
		if _, err := h.up.VerifyDownload(token); !errors.Is(err, uploads.ErrLinkExpired) {
			t.Fatalf("a second later: %v", err)
		}
	})
}

// target is an in-memory upload table for the sweeper.
type target struct {
	rows    map[uuid.UUID]*row
	marked  []uuid.UUID
	failDue bool
}

type row struct {
	keys        []string
	deleteAfter time.Time
	deletedAt   *time.Time
}

func (tg *target) Due(_ context.Context, now time.Time, limit int) ([]uploads.Due, error) {
	if tg.failDue {
		return nil, errors.New("database down")
	}
	var due []uploads.Due
	for id, r := range tg.rows {
		if r.deletedAt == nil && !r.deleteAfter.After(now) && len(due) < limit {
			due = append(due, uploads.Due{ID: id, Keys: r.keys})
		}
	}
	return due, nil
}

func (tg *target) MarkDeleted(_ context.Context, id uuid.UUID, at time.Time) error {
	tg.rows[id].deletedAt = &at
	tg.marked = append(tg.marked, id)
	return nil
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	finished, live, failing := uuid.New(), uuid.New(), uuid.New()
	tg := &target{rows: map[uuid.UUID]*row{}}
	for _, id := range []uuid.UUID{finished, live, failing} {
		keys := []string{uploads.QuarantineKey(tenant, id), uploads.FileKey(uploads.Transient, tenant, id), uploads.ErrorReportKey(uploads.Transient, tenant, id)}
		for _, k := range keys[1:] {
			_ = h.store.Put(ctx, k, []byte("sealed"))
		}
		tg.rows[id] = &row{keys: keys, deleteAfter: start.Add(48 * time.Hour)}
	}
	tg.rows[live].deleteAfter = start.Add(72 * time.Hour) // e.g. still processing, reset later
	h.store.FailDeletes(uploads.FileKey(uploads.Transient, tenant, failing), true)

	h.clock.Set(start.Add(48*time.Hour - time.Second))
	if res, err := h.up.Sweep(ctx, tg, 100); err != nil || res != (uploads.SweepResult{}) {
		t.Fatalf("a second early: %+v, %v", res, err)
	}
	if len(h.store.Keys()) != 6 {
		t.Fatalf("deleted early: %v", h.store.Keys())
	}

	h.clock.Set(start.Add(48 * time.Hour))
	res, err := h.up.Sweep(ctx, tg, 100)
	if err != nil || res.Deleted != 1 || res.Failed != 1 {
		t.Fatalf("at delete_after: %+v, %v", res, err)
	}
	if tg.rows[finished].deletedAt == nil || !tg.rows[finished].deletedAt.Equal(start.Add(48*time.Hour)) {
		t.Error("the finished upload isn't marked deleted at the sweep's time")
	}
	if tg.rows[failing].deletedAt != nil {
		t.Error("an upload whose file couldn't be deleted was marked deleted")
	}
	if tg.rows[live].deletedAt != nil {
		t.Error("an upload not yet due was swept")
	}
	for _, k := range tg.rows[finished].keys {
		if _, ok := h.store.Object(k); ok {
			t.Errorf("%s survived", k)
		}
	}

	// Again: nothing more changes; the failed one is retried and now goes.
	h.store.FailDeletes(uploads.FileKey(uploads.Transient, tenant, failing), false)
	res, err = h.up.Sweep(ctx, tg, 100)
	if err != nil || res.Deleted != 1 || res.Failed != 0 || len(tg.marked) != 2 {
		t.Fatalf("second sweep: %+v, %v, marked %d", res, err, len(tg.marked))
	}
	res, err = h.up.Sweep(ctx, tg, 100)
	if err != nil || res != (uploads.SweepResult{}) || len(tg.marked) != 2 {
		t.Fatalf("third sweep changed something: %+v, %v", res, err)
	}

	tg.failDue = true
	if _, err := h.up.Sweep(ctx, tg, 100); err == nil {
		t.Fatal("a failing table read was swallowed")
	}
}
