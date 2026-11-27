package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"

	"examforge/internal/ir"
	"examforge/internal/render"
)

// AssetRecord is what the asset registry states about one stored image. Only
// StorageKey decides which object is read.
type AssetRecord struct {
	AssetID    string
	StorageKey string
	MediaType  string
	SHA256     string
}

// AssetLookup returns the server-side record for an asset ID. It reports
// ErrNotFound when the ID is unknown.
type AssetLookup interface {
	AssetRecord(ctx context.Context, assetID string) (AssetRecord, error)
}

// S3AssetResolver implements render.AssetResolver on top of an ObjectStore.
// The IR asset is only consulted for its ID and declared media type: `uri`
// and `raw_uri` are never read, so a crafted URI cannot select another object.
//
// The base resolver streams every request. ForRender returns a render-scoped
// resolver that downloads each asset at most once and holds no open body.
type S3AssetResolver struct {
	store   ObjectStore
	lookup  AssetLookup
	maxByte int64
}

var _ render.AssetResolver = (*S3AssetResolver)(nil)

// NewS3AssetResolver builds a resolver. maxBytes bounds one asset; 0 selects
// MaxAssetBytes, matching the renderer's own per-asset limit.
func NewS3AssetResolver(store ObjectStore, lookup AssetLookup, maxBytes int64) (*S3AssetResolver, error) {
	if store == nil || lookup == nil {
		return nil, errors.New("storage: resolver needs an object store and an asset lookup")
	}
	if maxBytes < 0 {
		return nil, errors.New("storage: maxBytes must not be negative")
	}
	if maxBytes == 0 {
		maxBytes = MaxAssetBytes
	}
	return &S3AssetResolver{store: store, lookup: lookup, maxByte: maxBytes}, nil
}

// Resolve opens a bounded stream for the asset's current object. The stream
// verifies the registered SHA-256 while it is read and fails at EOF on a
// mismatch, so an object replaced after registration is never rendered as if
// it were the registered content.
func (r *S3AssetResolver) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("resolve asset %q: %w", assetID, err)
	}
	if err := ValidateAssetID(assetID); err != nil {
		return nil, fmt.Errorf("%w: %v", render.ErrAsset, err)
	}
	if asset.AssetID != assetID {
		return nil, fmt.Errorf("%w: asset %q was requested as %q", render.ErrAsset, asset.AssetID, assetID)
	}
	record, err := r.lookup.AssetRecord(ctx, assetID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%w: asset %q is not registered", render.ErrAsset, assetID)
	case err != nil:
		return nil, transientOrAsset(err, assetID)
	case record.AssetID != assetID:
		return nil, fmt.Errorf("%w: asset lookup returned %q for %q", render.ErrAsset, record.AssetID, assetID)
	case !ValidSHA256(record.SHA256):
		return nil, fmt.Errorf("%w: asset %q has no valid registered digest", render.ErrAsset, assetID)
	}
	// The key comes from the registry. It is validated anyway: a stored key
	// is data and must still be a canonical server key.
	if err := ValidateKey(record.StorageKey); err != nil {
		return nil, fmt.Errorf("%w: asset %q has an invalid storage key: %v", render.ErrAsset, assetID, err)
	}
	if declared := baseMediaType(asset.MediaType); declared != "" && declared != baseMediaType(record.MediaType) {
		return nil, fmt.Errorf("%w: asset %q is declared %q but registered as %q",
			render.ErrAsset, assetID, asset.MediaType, record.MediaType)
	}
	body, err := r.store.Get(ctx, record.StorageKey, r.maxByte)
	if err != nil {
		return nil, transientOrAsset(err, assetID)
	}
	return newDigestReader(body, record.SHA256, assetID), nil
}

// transientOrAsset maps an object store error to the renderer's classes. A
// transient store problem stays errors.Is(err, ErrUnavailable) so callers can
// retry it; everything else is a permanent asset error.
func transientOrAsset(err error, assetID string) error {
	switch {
	case errors.Is(err, ErrTooLarge):
		return fmt.Errorf("%w: asset %q: %w", render.ErrAssetTooLarge, assetID, err)
	case errors.Is(err, ErrUnavailable):
		return fmt.Errorf("%w: asset %q: %w", render.ErrAsset, assetID, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: asset %q has no object in the store: %w", render.ErrAsset, assetID, ErrNotFound)
	}
	return fmt.Errorf("%w: asset %q: %v", render.ErrAsset, assetID, err)
}

func baseMediaType(mediaType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
}

// digestReader hashes the stream and replaces EOF with an error when the
// bytes do not match the registered digest.
type digestReader struct {
	rc        io.ReadCloser
	sum       hash.Hash
	want      string
	assetID   string
	closeOnce sync.Once
	closeErr  error
}

func newDigestReader(rc io.ReadCloser, want, assetID string) *digestReader {
	return &digestReader{rc: rc, sum: sha256.New(), want: want, assetID: assetID}
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.rc.Read(p)
	if n > 0 {
		d.sum.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && hex.EncodeToString(d.sum.Sum(nil)) != d.want {
		return n, fmt.Errorf("%w: asset %q content does not match its registered SHA-256",
			render.ErrAsset, d.assetID)
	}
	return n, err
}

func (d *digestReader) Close() error {
	d.closeOnce.Do(func() { d.closeErr = d.rc.Close() })
	return d.closeErr
}

// renderScope cache limits. They mirror the renderer's per-render asset
// budget so a scope never holds more than a render would read.
const (
	DefaultMaxCachedAssets = 256
	DefaultMaxCachedBytes  = 64 << 20
)

// ForRender returns a resolver scoped to one render. Each asset ID is
// downloaded once even when it is requested many times or concurrently; the
// cache is bounded by entry count and total bytes and is dropped with the
// scope. Assets that do not fit the budget are streamed instead of cached.
func (r *S3AssetResolver) ForRender() render.AssetResolver {
	return &renderScope{base: r, cache: map[string][]byte{},
		maxAssets: DefaultMaxCachedAssets, maxBytes: DefaultMaxCachedBytes}
}

var _ render.AssetResolver = (*renderScope)(nil)

type renderScope struct {
	base      *S3AssetResolver
	inflight  singleflight.Group
	maxAssets int
	maxBytes  int64
	mu        sync.Mutex
	cache     map[string][]byte
	used      int64
}

func (s *renderScope) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	if err := ValidateAssetID(assetID); err != nil {
		return nil, fmt.Errorf("%w: %v", render.ErrAsset, err)
	}
	if data := s.peek(assetID); data != nil {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	value, err, _ := s.inflight.Do(assetID, func() (any, error) {
		if data := s.peek(assetID); data != nil {
			return data, nil
		}
		rc, err := s.base.Resolve(ctx, assetID, asset)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		s.admit(assetID, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(value.([]byte))), nil
}

func (s *renderScope) peek(assetID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[assetID]
}

// admit caches data when it fits both the entry and the byte budget.
func (s *renderScope) admit(assetID string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cache[assetID]; exists {
		return
	}
	if len(s.cache) >= s.maxAssets || s.used+int64(len(data)) > s.maxBytes {
		return
	}
	s.cache[assetID] = data
	s.used += int64(len(data))
}

// Cached returns the number of cached assets and bytes; tests use it.
func (s *renderScope) Cached() (assets int, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cache), s.used
}
