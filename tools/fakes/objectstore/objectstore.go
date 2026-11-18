// Package objectstore fakes platform/objectstore in memory (task 6.2a),
// so platform/uploads' rules can be tested without a container, with
// failures and a file swapped mid-read on demand. The real store is
// tested against SeaweedFS.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	real "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
)

// Store is an in-memory bucket.
type Store struct {
	mu          sync.Mutex
	objects     map[string][]byte
	failDeletes map[string]bool
	onGet       func(key string, count int)
	gets        map[string]int
}

// New is an empty bucket.
func New() *Store {
	return &Store{objects: map[string][]byte{}, failDeletes: map[string]bool{}, gets: map[string]int{}}
}

// Put stores data at key.
func (s *Store) Put(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = bytes.Clone(data)
	return nil
}

// Object returns what's stored at key, and whether anything is.
func (s *Store) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return bytes.Clone(b), ok
}

// Keys lists every stored key, sorted.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FailDeletes makes deleting key fail until called again with false.
func (s *Store) FailDeletes(key string, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failDeletes[key] = fail
}

// OnGet runs f after each GetStream opens a key, with how many times
// that key has been opened: a test can swap the file between reads.
func (s *Store) OnGet(f func(key string, count int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onGet = f
}

// Stat reports the object at key.
func (s *Store) Stat(_ context.Context, key string) (real.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return real.ObjectInfo{}, real.ErrNotFound
	}
	return real.ObjectInfo{Size: int64(len(b))}, nil
}

// PutStream stores what r yields; a known size must match.
func (s *Store) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return errors.New("fake objectstore: stream length differs from the declared size")
	}
	return s.Put(ctx, key, data)
}

// GetStream opens the object at key.
func (s *Store) GetStream(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	b, ok := s.objects[key]
	s.gets[key]++
	count, hook := s.gets[key], s.onGet
	s.mu.Unlock()
	if !ok {
		return nil, real.ErrNotFound
	}
	if hook != nil {
		hook(key, count)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Delete removes key; a missing key isn't an error.
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failDeletes[key] {
		return errors.New("fake objectstore: delete failed")
	}
	delete(s.objects, key)
	return nil
}

// PresignUpload returns a form naming the key, the limit and the
// expiry, signed by nobody.
func (s *Store) PresignUpload(_ context.Context, key string, maxBytes int64, expiresAt time.Time) (real.UploadForm, error) {
	return real.UploadForm{
		URL: "https://storage.invalid/bucket",
		Fields: map[string]string{
			"key":     key,
			"x-limit": strconv.FormatInt(maxBytes, 10),
			"x-until": expiresAt.UTC().Format(time.RFC3339),
		},
	}, nil
}

