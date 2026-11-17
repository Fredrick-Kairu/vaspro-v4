package uploads

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

// Download is what a valid download link names.
type Download struct {
	Tenant uuid.UUID
	Key    string
}

// SignDownload makes a short-lived link token for one stored file. The
// service signs only after finding the upload under the caller's own
// tenant, and serves the token from its own route, decrypting as it
// streams: storage only ever holds ciphertext, so a storage link would
// hand out nothing useful (ADR 0012).
//
// Token: base64url(tenant | uint64 expiry, Unix seconds | key), ".",
// base64url(HMAC-SHA256 of the first part under the download key).
func (u *Uploads) SignDownload(tenant uuid.UUID, key string) (string, time.Time, error) {
	if key == "" {
		return "", time.Time{}, errors.New("uploads: no key to sign")
	}
	expires := u.clock.Now().Add(u.cfg.DownloadLinkTTL).Truncate(time.Second)
	payload := make([]byte, 0, 16+8+len(key))
	payload = append(payload, tenant[:]...)
	payload = binary.BigEndian.AppendUint64(payload, uint64(expires.Unix())) //nolint:gosec // G115: a time after 1970
	payload = append(payload, key...)
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := crypto.HMACSign(u.cfg.DownloadKey, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac), expires, nil
}

// VerifyDownload checks a token and returns what it names:
// ErrLinkInvalid for anything not signed by this key, ErrLinkExpired
// after its time.
func (u *Uploads) VerifyDownload(token string) (Download, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || strings.Contains(sig, ".") {
		return Download{}, ErrLinkInvalid
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil || !crypto.HMACVerify(u.cfg.DownloadKey, []byte(body), mac) {
		return Download{}, ErrLinkInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil || len(payload) <= 24 {
		return Download{}, ErrLinkInvalid
	}
	tenant, err := uuid.FromBytes(payload[:16])
	if err != nil {
		return Download{}, ErrLinkInvalid
	}
	expires := time.Unix(int64(binary.BigEndian.Uint64(payload[16:24])), 0) //nolint:gosec // G115: signed by us
	if u.clock.Now().After(expires) {
		return Download{}, ErrLinkExpired
	}
	return Download{Tenant: tenant, Key: string(bytes.Clone(payload[24:]))}, nil
}
