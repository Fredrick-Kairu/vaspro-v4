//go:build integration

package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

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
