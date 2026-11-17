package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
)

// File is what Finalize learned about a stored file.
type File struct {
	Size   int64
	SHA256 []byte // of the plaintext
}

// Finalize moves the upload of id from quarantine to its final key
// (data model §8's "scanning" step):
//  1. size: nothing, or more than the cap, is refused;
//  2. a first read checks the type from the content, takes the SHA-256
//     and streams the file to the virus scanner, writing nothing;
//  3. a second read encrypts it to its final key, hashing again: if the
//     hash differs, the file was replaced between the reads (the upload
//     form still works for a few minutes), and it's refused;
//  4. the quarantine copy is deleted.
//
// A refused file is deleted at once, with anything written for it. If
// the scanner can't answer, the quarantine copy stays for a retry.
// Neither read holds the file in memory.
func (u *Uploads) Finalize(ctx context.Context, class Class, tenant, id uuid.UUID, want ContentType) (File, error) {
	qkey, fkey := QuarantineKey(tenant, id), FileKey(class, tenant, id)
	info, err := u.store.Stat(ctx, qkey)
	if errors.Is(err, objectstore.ErrNotFound) {
		if _, ferr := u.store.Stat(ctx, fkey); ferr == nil {
			return File{}, ErrAlreadyFinalized
		}
		return File{}, ErrNotUploaded
	}
	if err != nil {
		return File{}, fmt.Errorf("uploads: checking the upload: %w", err)
	}
	switch {
	case info.Size == 0:
		return File{}, u.refuse(ctx, ErrEmpty, qkey)
	case info.Size > u.cfg.MaxBytes:
		return File{}, u.refuse(ctx, ErrTooLarge, qkey)
	}

	sum, err := u.checkQuarantined(ctx, qkey, info.Size, want)
	if err != nil {
		if errors.Is(err, ErrScanUnavailable) {
			return File{}, err
		}
		return File{}, u.refuse(ctx, err, qkey)
	}

	sealer, err := u.envelope.NewStreamSealer(ctx, u.cfg.KMSKeyID)
	if err != nil {
		return File{}, fmt.Errorf("uploads: sealing: %w", err)
	}
	src, err := u.store.GetStream(ctx, qkey)
	if err != nil {
		return File{}, fmt.Errorf("uploads: reading the upload: %w", err)
	}
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(io.LimitReader(src, info.Size+1), h)}
	err = u.putSealed(ctx, fkey, sealer, counted, info.Size)
	_ = src.Close()
	if counted.n != info.Size || !bytes.Equal(h.Sum(nil), sum) {
		return File{}, u.refuse(ctx, ErrChanged, qkey, fkey)
	}
	if err != nil {
		return File{}, err
	}
	if err := u.store.Delete(ctx, qkey); err != nil {
		return File{}, fmt.Errorf("uploads: deleting the quarantine copy: %w", err)
	}
	return File{Size: info.Size, SHA256: sum}, nil
}

// checkQuarantined is Finalize's first read: type, hash and scan.
func (u *Uploads) checkQuarantined(ctx context.Context, qkey string, size int64, want ContentType) ([]byte, error) {
	src, err := u.store.GetStream(ctx, qkey)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the upload: %v", ErrScanUnavailable, err)
	}
	defer func() { _ = src.Close() }()
	h := sha256.New()
	limited := io.LimitReader(src, size+1)
	head := make([]byte, min(size, sniffSize))
	if _, err := io.ReadFull(limited, head); err != nil {
		return nil, ErrChanged
	}
	if !matchesType(want, head) {
		return nil, ErrWrongType
	}
	counted := &countingReader{r: io.TeeReader(io.MultiReader(bytes.NewReader(head), limited), hashWriter{h})}
	res, err := u.scanner.Scan(ctx, counted)
	switch {
	case errors.Is(err, clamav.ErrTooLarge):
		return nil, fmt.Errorf("%w: over the scanner's limit", ErrTooLarge)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrScanUnavailable, err)
	case !res.Clean:
		return nil, fmt.Errorf("%w: %s", ErrInfected, res.Signature)
	case counted.n != size:
		return nil, ErrChanged
	}
	return h.Sum(nil), nil
}

// refuse deletes what was kept for a refused upload and returns cause.
func (u *Uploads) refuse(ctx context.Context, cause error, keys ...string) error {
	for _, k := range keys {
		if err := u.store.Delete(ctx, k); err != nil {
			return fmt.Errorf("%w (and deleting it failed: %v)", cause, err)
		}
	}
	return cause
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type hashWriter struct{ h hash.Hash }

func (w hashWriter) Write(p []byte) (int, error) { return w.h.Write(p) }
