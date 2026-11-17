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
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// ErrNotFound means there's no object at that key.
var ErrNotFound = errors.New("objectstore: not found")

// Config is where the store is. AccessKey and SecretKey may be empty
// for a store that allows anonymous access (local SeaweedFS); signed
// upload forms need them.
type Config struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Bucket    string
	UseTLS    bool

	// PublicEndpoint (host:port) is how browsers reach the store, for
	// signed upload forms (task 6.2a, D-23). A signature covers the
	// host, so forms are signed for this one; empty means Endpoint.
	PublicEndpoint string
	PublicUseTLS   bool
	// Region is the store's S3 region, set so signing never asks the
	// store; empty means us-east-1.
	Region string
}

// Store is one bucket of an S3-compatible object store.
type Store struct {
	client *minio.Client
	public *minio.Client // signs forms for PublicEndpoint
	bucket string
}

// partSize is how much an upload of unknown length buffers per part,
// so streaming a large file holds 8 MB at a time, not the whole file.
const partSize = 8 << 20

// New builds a Store; it doesn't connect until first used.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objectstore: an endpoint and a bucket are required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	newClient := func(endpoint string, tls bool) (*minio.Client, error) {
		opts := &minio.Options{Secure: tls, Region: region}
		if cfg.AccessKey != "" {
			opts.Creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
		}
		client, err := minio.New(endpoint, opts)
		if err != nil {
			return nil, fmt.Errorf("objectstore: building client: %w", err)
		}
		return client, nil
	}
	client, err := newClient(cfg.Endpoint, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	public := client
	if cfg.PublicEndpoint != "" {
		if public, err = newClient(cfg.PublicEndpoint, cfg.PublicUseTLS); err != nil {
			return nil, err
		}
	}
	return &Store{client: client, public: public, bucket: cfg.Bucket}, nil
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
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objectstore: reading %s: %w", key, err)
	}
	return data, nil
}

func isNotFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}

// ObjectInfo is what Stat reports about an object.
type ObjectInfo struct {
	Size int64
}

// Stat reports the object at key, or ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	return ObjectInfo{Size: info.Size}, nil
}

// PutStream stores what r yields at key without holding it all: size
// is its exact length, or -1 if unknown.
func (s *Store) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: partSize}); err != nil {
		return fmt.Errorf("objectstore: putting %s: %w", key, err)
	}
	return nil
}

// GetStream opens the object at key for reading, or ErrNotFound. The
// caller closes it.
func (s *Store) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: getting %s: %w", key, err)
	}
	// GetObject is lazy: Stat makes a missing key fail here, not on the
	// first read.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objectstore: getting %s: %w", key, err)
	}
	return obj, nil
}

// UploadForm is a signed S3 POST form: a browser posts Fields, then the
// file in a field named "file", to URL.
type UploadForm struct {
	URL    string
	Fields map[string]string
}

// PresignUpload signs a form that lets its holder upload one file of
// at most maxBytes to exactly key until expiresAt. The store itself
// refuses anything else, so nothing is trusted from the uploader.
func (s *Store) PresignUpload(ctx context.Context, key string, maxBytes int64, expiresAt time.Time) (UploadForm, error) {
	policy := minio.NewPostPolicy()
	for _, err := range []error{
		policy.SetBucket(s.bucket),
		policy.SetKey(key),
		policy.SetExpires(expiresAt),
		policy.SetContentLengthRange(1, maxBytes),
	} {
		if err != nil {
			return UploadForm{}, fmt.Errorf("objectstore: building upload policy: %w", err)
		}
	}
	u, fields, err := s.public.PresignedPostPolicy(ctx, policy)
	if err != nil {
		return UploadForm{}, fmt.Errorf("objectstore: signing upload form: %w", err)
	}
	return UploadForm{URL: u.String(), Fields: fields}, nil
}

// ErrExpiryUnsupported means the store refused a bucket expiry rule.
var ErrExpiryUnsupported = errors.New("objectstore: the store doesn't support expiry rules")

// SetExpiryRule asks the store to delete objects under prefix days
// after they're written: a backstop behind a service's own sweeper
// (data model §8). It replaces the bucket's rules.
func (s *Store) SetExpiryRule(ctx context.Context, prefix string, days int) error {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{{
		ID:         "expire-" + strings.TrimSuffix(prefix, "/"),
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: prefix},
		Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)},
	}}
	if err := s.client.SetBucketLifecycle(ctx, s.bucket, cfg); err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == "NotImplemented" || code == "MethodNotAllowed" {
			return fmt.Errorf("%w: %s", ErrExpiryUnsupported, code)
		}
		return fmt.Errorf("objectstore: setting expiry rule: %w", err)
	}
	return nil
}

// Delete removes the object at key; a missing one isn't an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: deleting %s: %w", key, err)
	}
	return nil
}
