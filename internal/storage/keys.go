// Package storage is the object-storage boundary: an S3-protocol client with
// path-style addressing forced on, deterministic key rules, bounded reads,
// immutable puts, an in-memory fake, and the renderer asset resolver.
//
// The IR asset `uri` is never used as a key. Keys are always built here from
// validated IDs and server-chosen constants.
package storage

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Size limits. One source PDF and one put are at most 64 MiB; one asset is at
// most 32 MiB, matching the renderer's per-asset limit.
const (
	MaxSourceBytes = 64 << 20
	MaxPutBytes    = 64 << 20
	MaxAssetBytes  = 32 << 20
)

// Object store error classes. Wrap or test with errors.Is.
var (
	// ErrNotFound reports a missing key.
	ErrNotFound = errors.New("object not found")
	// ErrForbidden reports a key or permission problem that retrying cannot fix.
	ErrForbidden = errors.New("object store rejected the request")
	// ErrInvalidKey reports a key that is not a canonical server key.
	ErrInvalidKey = errors.New("invalid object key")
	// ErrTooLarge reports a body or object above its limit.
	ErrTooLarge = errors.New("object exceeds size limit")
	// ErrConflict reports an existing object with different content.
	ErrConflict = errors.New("object already exists with different content")
	// ErrInvalid reports a caller mistake (negative size, bad media type).
	ErrInvalid = errors.New("invalid object store request")
	// ErrUnavailable reports a transient service or network problem.
	ErrUnavailable = errors.New("object store unavailable")
)

// maxKeyBytes bounds a whole key.
const maxKeyBytes = 1024

var (
	// keySegment is one path element: starts alphanumeric, then a restricted
	// set. "." and ".." are rejected by the leading-alphanumeric rule, and
	// "%", "\" and control characters are outside the set.
	keySegment  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	assetIDRe   = regexp.MustCompile(`^ast_[A-Za-z0-9_-]{1,124}$`)
	mediaTypeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*$`)
)

// ValidateKey accepts only canonical keys: relative, non-empty, no empty, "."
// or ".." segment, no backslash, NUL, control character, percent-encoding or
// duplicated separator.
func ValidateKey(key string) error {
	switch {
	case key == "" || len(key) > maxKeyBytes:
		return fmt.Errorf("%w: length must be 1 to %d", ErrInvalidKey, maxKeyBytes)
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("%w: must not be absolute", ErrInvalidKey)
	case strings.ContainsAny(key, "\\\x00%") || strings.ContainsFunc(key, isControl):
		return fmt.Errorf("%w: contains a forbidden character", ErrInvalidKey)
	case strings.Contains(key, "//"):
		return fmt.Errorf("%w: contains an empty segment", ErrInvalidKey)
	}
	for _, seg := range strings.Split(key, "/") {
		if !keySegment.MatchString(seg) {
			return fmt.Errorf("%w: segment %q is not canonical", ErrInvalidKey, seg)
		}
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// ValidateAssetID accepts the IR asset ID shape.
func ValidateAssetID(id string) error {
	if !assetIDRe.MatchString(id) {
		return fmt.Errorf("%w: asset ID %q is not ast_[A-Za-z0-9_-]+", ErrInvalid, id)
	}
	return nil
}

// ExtForMediaType maps a supported image media type to its file extension.
// Parameters such as "; charset=binary" are ignored.
func ExtForMediaType(mediaType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])) {
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpg", nil
	case "image/gif":
		return "gif", nil
	case "image/svg+xml":
		return "svg", nil
	case "image/webp":
		return "webp", nil
	}
	return "", fmt.Errorf("%w: media type %q is not a supported image type", ErrInvalid, mediaType)
}

// SourceKey is the key of one job's uploaded source PDF. The job ID is a UUID,
// so the key carries no user-controlled path fragment.
func SourceKey(jobID uuid.UUID) string {
	return "uploads/" + jobID.String() + "/source.pdf"
}

// RawAssetKey is the key of an asset's original crop.
func RawAssetKey(assetID, mediaType string) (string, error) {
	return assetKey("raw", assetID, mediaType)
}

// CleanAssetKey is the key of an asset's processed image.
func CleanAssetKey(assetID, mediaType string) (string, error) {
	return assetKey("clean", assetID, mediaType)
}

func assetKey(variant, assetID, mediaType string) (string, error) {
	if err := ValidateAssetID(assetID); err != nil {
		return "", err
	}
	ext, err := ExtForMediaType(mediaType)
	if err != nil {
		return "", err
	}
	return "assets/" + variant + "/" + assetID + "." + ext, nil
}

// JobKeyPrefix is the recognizable prefix of every object a job may leave.
func JobKeyPrefix(jobID uuid.UUID) string { return "uploads/" + jobID.String() + "/" }
