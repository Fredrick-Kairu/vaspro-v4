// Package objectstore is S3-compatible object storage (system design:
// "Documents: encrypted S3-compatible object storage"; SeaweedFS's S3
// gateway locally, task 2.8). It stores bytes as given: callers encrypt
// anything sensitive before Put (accounts seals documents with
// platform/crypto.Envelope), so the bucket only ever holds ciphertext.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound means there's no object at that key.
var ErrNotFound = errors.New("objectstore: not found")

// Config is where the store is. AccessKey and SecretKey may be empty
// for a store that allows anonymous access (local SeaweedFS).
type Config struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Bucket    string
	UseTLS    bool
}

// Store is one bucket of an S3-compatible object store.
type Store struct {
	client *minio.Client
	bucket string
}

// New builds a Store; it doesn't connect until first used.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objectstore: an endpoint and a bucket are required")
	}
	opts := &minio.Options{Secure: cfg.UseTLS}
	if cfg.AccessKey != "" {
		opts.Creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("objectstore: building client: %w", err)
	}
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// EnsureBucket creates the bucket if it doesn't exist yet.
func (s *Store) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("objectstore: checking bucket %s: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("objectstore: creating bucket %s: %w", s.bucket, err)
	}
	return nil
}

// Put stores data at key, replacing anything there.
func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	if _, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("objectstore: putting %s: %w", key, err)
	}
	return nil
}

// Get returns the object at key, or ErrNotFound.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: getting %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objectstore: reading %s: %w", key, err)
	}
	return data, nil
}

// Delete removes the object at key; a missing one isn't an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: deleting %s: %w", key, err)
	}
	return nil
}
