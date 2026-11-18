//go:build integration

package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestStore_PutGetDelete(t *testing.T) {
	ctx := context.Background()
	cfg := testkit.StartObjectStorage(t)
	cfg.Bucket = "test-documents"
	store, err := objectstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket again: %v", err)
	}

	data := []byte("sealed bytes, as accounts would store them")
	if err := store.Put(ctx, "tenants/a/documents/1", data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, "tenants/a/documents/1")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get: %q, %v", got, err)
	}
	if _, err := store.Get(ctx, "tenants/a/documents/missing"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("a missing object: got %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "tenants/a/documents/1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "tenants/a/documents/1"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("after Delete: got %v, want ErrNotFound", err)
	}
}

func newStore(t *testing.T, bucket string) (*objectstore.Store, objectstore.Config) {
	t.Helper()
	cfg := testkit.StartObjectStorage(t)
	cfg.Bucket = bucket
	cfg.PublicEndpoint = cfg.Endpoint // the test reaches storage the way a browser would
	store, err := objectstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, cfg
}

func TestStore_Streams(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t, "test-streams")

	// 20 MB with an unknown length goes up in parts; a known length too.
	data := bytes.Repeat([]byte("0123456789abcdef"), 20<<20/16)
	for _, size := range []int64{int64(len(data)), -1} {
		if err := store.PutStream(ctx, "s/1", bytes.NewReader(data), size); err != nil {
			t.Fatalf("PutStream(size %d): %v", size, err)
		}
		info, err := store.Stat(ctx, "s/1")
		if err != nil || info.Size != int64(len(data)) {
			t.Fatalf("Stat: %+v, %v", info, err)
		}
		r, err := store.GetStream(ctx, "s/1")
		if err != nil {
			t.Fatalf("GetStream: %v", err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("GetStream read %d bytes, %v", len(got), err)
		}
	}
	if _, err := store.Stat(ctx, "s/missing"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("Stat of a missing object: %v", err)
	}
	if _, err := store.GetStream(ctx, "s/missing"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("GetStream of a missing object: %v", err)
	}
	if err := store.Delete(ctx, "s/missing"); err != nil {
		t.Fatalf("deleting a missing object: %v", err)
	}
}

// postForm uploads body as a browser would with a signed form: the
// form's fields, then the file last.
func postForm(t *testing.T, form objectstore.UploadForm, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range form.Fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", "contacts.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(body)
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, form.URL, &buf)
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

func TestStore_PresignedUpload(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t, "test-presign")
	expires := time.Now().Add(15 * time.Minute)

	t.Run("an upload within the limits lands at the signed key", func(t *testing.T) {
		form, err := store.PresignUpload(ctx, "quarantine/a/1", 1024, expires)
		if err != nil {
			t.Fatalf("PresignUpload: %v", err)
		}
		if code := postForm(t, form, []byte("name\nTest\n")); code/100 != 2 {
			t.Fatalf("upload answered %d", code)
		}
		got, err := store.Get(ctx, "quarantine/a/1")
		if err != nil || string(got) != "name\nTest\n" {
			t.Fatalf("stored %q, %v", got, err)
		}
	})

	t.Run("over the size limit is refused by the store", func(t *testing.T) {
		form, err := store.PresignUpload(ctx, "quarantine/a/2", 1024, expires)
		if err != nil {
			t.Fatal(err)
		}
		if code := postForm(t, form, bytes.Repeat([]byte("x"), 1025)); code/100 == 2 {
			t.Fatalf("1,025 bytes against a 1,024 limit answered %d", code)
		}
		if _, err := store.Stat(ctx, "quarantine/a/2"); !errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("the oversize upload was stored: %v", err)
		}
	})

	t.Run("the form can't write another key", func(t *testing.T) {
		form, err := store.PresignUpload(ctx, "quarantine/a/3", 1024, expires)
		if err != nil {
			t.Fatal(err)
		}
		form.Fields["key"] = "uploads/transient/a/3"
		if code := postForm(t, form, []byte("x")); code/100 == 2 {
			t.Fatalf("a changed key answered %d", code)
		}
		if _, err := store.Stat(ctx, "uploads/transient/a/3"); !errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("the upload reached a key it wasn't signed for: %v", err)
		}
	})

	t.Run("an expired form is refused", func(t *testing.T) {
		form, err := store.PresignUpload(ctx, "quarantine/a/4", 1024, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if code := postForm(t, form, []byte("x")); code/100 == 2 {
			t.Fatalf("an expired form answered %d", code)
		}
	})

	t.Run("an unsigned post is refused", func(t *testing.T) {
		form, err := store.PresignUpload(ctx, "quarantine/a/5", 1024, expires)
		if err != nil {
			t.Fatal(err)
		}
		for k := range form.Fields {
			if strings.Contains(strings.ToLower(k), "signature") {
				form.Fields[k] = "00" + form.Fields[k][2:]
			}
		}
		if code := postForm(t, form, []byte("x")); code/100 == 2 {
			t.Fatalf("a bad signature answered %d", code)
		}
	})
}

// TestStore_ExpiryRuleSupport records whether the store accepts a
// bucket expiry rule, data model §8's open point on the backstop.
// SeaweedFS's answer decides whether the sweeper is the only deleter.
func TestStore_ExpiryRuleSupport(t *testing.T) {
	store, _ := newStore(t, "test-expiry")
	err := store.SetExpiryRule(context.Background(), "quarantine/", 1)
	if err == nil {
		t.Log("SeaweedFS accepted a bucket expiry rule")
	} else {
		t.Logf("SeaweedFS refused a bucket expiry rule: %v", err)
	}
	if err != nil && !errors.Is(err, objectstore.ErrExpiryUnsupported) {
		t.Fatalf("SetExpiryRule failed other than as unsupported: %v", err)
	}
}
