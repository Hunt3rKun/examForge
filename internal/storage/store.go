package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ObjectInfo describes one stored object. ETag is the server value when it
// provides one; it is never used to decide content equality. Metadata is the
// user metadata of a HEAD response and is empty in listings.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
	ETag        string
	Metadata    map[string]string
}

// ObjectStore is the minimal object-storage contract. Implementations:
// *S3Store (S3 protocol, path-style) and *MemStore (offline fake).
//
//   - Put writes exactly size bytes and never replaces different content: an
//     existing key is hashed first, equal bytes are an idempotent success and
//     different bytes are ErrConflict with the original left untouched.
//   - Get bounds the returned stream: reading more than maxBytes fails with
//     ErrTooLarge. The caller closes the reader.
//   - Stat reports ErrNotFound for a missing key; Exists is a Stat call.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string, maxBytes int64) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// creator stores bytes under a key that must not exist yet. It reports
// errAlreadyExists when a concurrent writer won the race.
type creator interface {
	create(ctx context.Context, key string, data []byte, sha256, contentType string) error
}

// errAlreadyExists reports a create that lost a race to a concurrent writer.
var errAlreadyExists = errors.New("object already exists")

// ObjectLister is implemented by stores that can enumerate a key prefix. It
// makes orphan objects recognizable and removable.
type ObjectLister interface {
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// HashBytes returns the lowercase SHA-256 hex of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidSHA256 reports whether s is a 64-character lowercase hex digest.
func ValidSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// HashMetaKey is the user metadata under which an immutable put records the
// content digest, so later puts can skip re-downloading the object.
const HashMetaKey = "sha256"

// PutImmutable is the exported form of putImmutable: it stores exactly size
// bytes and never replaces different content.
func PutImmutable(ctx context.Context, store ObjectStore, key string, body io.Reader, size int64, contentType string, maxBytes int64) (string, error) {
	cr, ok := store.(creator)
	if !ok {
		return "", errors.New("object store does not support immutable create")
	}
	return putImmutable(ctx, store, cr, key, body, size, contentType, maxBytes)
}

// putImmutable stores exactly size bytes under key and never replaces
// different content. An existing object is verified by digest: equal content
// is an idempotent success, different content is ErrConflict and the stored
// object is left untouched.
//
// The body is read into memory (bounded by maxBytes), so callers bound the
// concurrency around this call.
func putImmutable(ctx context.Context, store ObjectStore, cr creator, key string, body io.Reader, size int64, contentType string, maxBytes int64) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := validateContentType(contentType); err != nil {
		return "", err
	}
	data, err := readExact(ctx, body, size, maxBytes)
	if err != nil {
		return "", err
	}
	sum := HashBytes(data)
	info, err := store.Stat(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound):
		if err := cr.create(ctx, key, data, sum, contentType); err != nil {
			if errors.Is(err, errAlreadyExists) {
				return verifyExisting(ctx, store, key, sum, maxBytes)
			}
			return "", err
		}
		return sum, nil
	case err != nil:
		return "", err
	}
	if info.Metadata != nil && info.Metadata[HashMetaKey] == sum {
		return sum, nil
	}
	return verifyExisting(ctx, store, key, sum, maxBytes)
}

func verifyExisting(ctx context.Context, store ObjectStore, key, sum string, maxBytes int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rc, err := store.Get(ctx, key, maxBytes)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			// Larger than anything this path would have written.
			return "", fmt.Errorf("%w: key %s", ErrConflict, key)
		}
		return "", err
	}
	defer rc.Close()
	existing, err := io.ReadAll(rc)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return "", fmt.Errorf("%w: key %s", ErrConflict, key)
		}
		return "", err
	}
	if HashBytes(existing) != sum {
		return "", fmt.Errorf("%w: key %s holds different content; the object was left unchanged", ErrConflict, key)
	}
	return sum, nil
}

// readExact reads exactly size bytes, refusing more, and fails as soon as
// size exceeds limit or the context is cancelled.
func readExact(ctx context.Context, r io.Reader, size, limit int64) ([]byte, error) {
	switch {
	case size < 0:
		return nil, fmt.Errorf("%w: size %d is negative", ErrInvalid, size)
	case limit <= 0 || size > limit:
		return nil, fmt.Errorf("%w: size %d, limit %d", ErrTooLarge, size, limit)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(io.LimitReader(r, size+1), buf); err != nil {
		return nil, fmt.Errorf("%w: body is shorter than the declared size %d: %v", ErrInvalid, size, err)
	}
	var extra [1]byte
	if n, _ := io.ReadFull(r, extra[:]); n != 0 {
		return nil, fmt.Errorf("%w: body is longer than the declared size %d", ErrInvalid, size)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return buf, nil
}

func validateContentType(contentType string) error {
	if contentType == "" || len(contentType) > 255 || !mediaTypeRe.MatchString(contentType) {
		return fmt.Errorf("%w: content type %q", ErrInvalid, contentType)
	}
	return nil
}

// boundedReader fails with ErrTooLarge as soon as more than max bytes have
// been read, and always closes the source.
type boundedReader struct {
	rc        io.ReadCloser
	remaining int64
	closed    bool
}

func newBoundedReader(rc io.ReadCloser, max int64) *boundedReader {
	return &boundedReader{rc: rc, remaining: max}
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

func (b *boundedReader) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	return b.rc.Close()
}

// Classify reports whether an object store error is retryable. Context
// cancellation follows the context: a cancelled context is never retried.
func Classify(err error) (transient bool, notFound bool) {
	return errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotFound)
}

var (
	// Any absolute URL is replaced whole: a stored message never carries an
	// endpoint, userinfo, query signature or object URL.
	urlRe         = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	secretShapeRe = regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)
	bearerRe      = regexp.MustCompile(`(?i)(authorization|secret|password|token|signature)\s*[=:]\s*[^\s,;&"']+`)
)

// Redact returns a bounded, single-line message safe to store and log. Every
// listed secret is replaced, URL userinfo and queries are dropped, and shaped
// credentials are masked. The result never exceeds maxRunes runes.
func Redact(msg string, maxRunes int, secrets ...string) string {
	out := strings.ReplaceAll(msg, "\n", " ")
	out = strings.ReplaceAll(out, "\r", " ")
	for _, secret := range secrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[redacted]")
		}
	}
	out = urlRe.ReplaceAllString(out, "[url]")
	out = secretShapeRe.ReplaceAllString(out, "[redacted]")
	out = bearerRe.ReplaceAllString(out, "$1=[redacted]")
	out = strings.TrimSpace(out)
	if utf8.RuneCountInString(out) > maxRunes {
		runes := []rune(out)
		out = string(runes[:maxRunes])
	}
	return out
}
