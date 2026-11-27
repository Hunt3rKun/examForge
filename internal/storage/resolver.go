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

// Variant names one served form of an asset. "active" follows the row's
// selector; "raw" and "clean" name a variant explicitly.
type Variant string

const (
	VariantActive Variant = "active"
	VariantRaw    Variant = "raw"
	VariantClean  Variant = "clean"
)

// ErrVariantUnavailable reports that an asset does not have a verified object
// for the requested variant: a raw_cropped asset has no clean variant, and a
// pre-digest legacy raw companion cannot be verified. Callers map it to "not
// found", never to an internal failure.
var ErrVariantUnavailable = errors.New("asset variant unavailable")

// AssetRecord is what the asset registry states about one stored image. Each
// variant has its own key and digest; ActiveVariant selects the one served for
// "active". StorageKey/SHA256 are the pre-variant main columns, retained for
// compatibility and still authoritative for the variant they described.
//
// RawSHA256 empty with a RawStorageKey present marks a pre-digest legacy row:
// the raw companion cannot be verified and is never served. The clean digest
// never verifies raw bytes.
type AssetRecord struct {
	AssetID         string
	MediaType       string
	Status          string
	StorageKey      string
	SHA256          string
	RawStorageKey   string
	RawSHA256       string
	CleanStorageKey string
	CleanSHA256     string
	ActiveVariant   Variant
	Revision        int64
}

// Select resolves the key and digest that serve variant. "active" follows the
// row's selector. An unavailable variant reports ErrVariantUnavailable; a row
// whose key/digest pair is incomplete or invalid is never served unverified.
func (r AssetRecord) Select(variant Variant) (key, digest string, err error) {
	switch variant {
	case VariantActive:
		switch r.ActiveVariant {
		case VariantRaw:
			return r.Select(VariantRaw)
		case VariantClean, "":
			return r.Select(VariantClean)
		}
		return "", "", fmt.Errorf("%w: asset %q has unknown active variant %q",
			ErrVariantUnavailable, r.AssetID, r.ActiveVariant)
	case VariantRaw:
		if r.RawStorageKey == "" {
			return "", "", fmt.Errorf("%w: asset %q has no raw object", ErrVariantUnavailable, r.AssetID)
		}
		if !ValidSHA256(r.RawSHA256) {
			return "", "", fmt.Errorf("%w: asset %q raw object has no registered digest",
				ErrVariantUnavailable, r.AssetID)
		}
		return r.RawStorageKey, r.RawSHA256, nil
	case VariantClean:
		// A row written before migration 0007 has no clean columns; for such a
		// row the main key is the clean object unless it is also the raw
		// object (a raw_cropped row, which has no clean variant at all).
		key, digest = r.CleanStorageKey, r.CleanSHA256
		if key == "" {
			if r.Status == "raw_cropped" || (r.RawStorageKey != "" && r.RawStorageKey == r.StorageKey) {
				return "", "", fmt.Errorf("%w: asset %q (%s) has no clean variant",
					ErrVariantUnavailable, r.AssetID, r.Status)
			}
			key, digest = r.StorageKey, r.SHA256
		}
		if key == "" || !ValidSHA256(digest) {
			return "", "", fmt.Errorf("%w: asset %q has no verified clean object", ErrVariantUnavailable, r.AssetID)
		}
		if r.RawStorageKey != "" && r.RawStorageKey == key {
			// The raw companion key must never double as the clean object.
			return "", "", fmt.Errorf("%w: asset %q clean and raw keys are the same object",
				ErrVariantUnavailable, r.AssetID)
		}
		return key, digest, nil
	}
	return "", "", fmt.Errorf("%w: asset %q: unknown variant %q", ErrVariantUnavailable, r.AssetID, variant)
}

// Available reports the variants that have a verified key and digest, in a
// stable order. A variant listed here can be selected and served.
func (r AssetRecord) Available() []Variant {
	out := make([]Variant, 0, 2)
	for _, v := range []Variant{VariantRaw, VariantClean} {
		if _, _, err := r.Select(v); err == nil {
			out = append(out, v)
		}
	}
	return out
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

// Resolve opens a bounded stream for the object the asset's active variant
// selects. The stream verifies the registered SHA-256 while it is read and
// fails at EOF on a mismatch, so an object replaced after registration is
// never rendered as if it were the registered content.
func (r *S3AssetResolver) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	return r.ResolveVariant(ctx, assetID, asset, VariantActive)
}

// ResolveVariant opens a bounded, digest-verified stream for one named variant.
// "active" follows the row's selector; "raw" and "clean" are explicit. A
// variant without a verified key and digest reports render.ErrAsset wrapping
// ErrVariantUnavailable, and no bytes are returned.
func (r *S3AssetResolver) ResolveVariant(ctx context.Context, assetID string, asset ir.Asset, variant Variant) (io.ReadCloser, error) {
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
	}
	return r.open(ctx, record, assetID, asset, variant)
}

// open validates one already-snapshotted record and opens its object. The key
// comes from the registry. It is validated anyway: a stored key is data and
// must still be a canonical server key.
func (r *S3AssetResolver) open(ctx context.Context, record AssetRecord, assetID string, asset ir.Asset, variant Variant) (io.ReadCloser, error) {
	key, digest, err := record.Select(variant)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", render.ErrAsset, err)
	}
	if err := ValidateKey(key); err != nil {
		return nil, fmt.Errorf("%w: asset %q has an invalid storage key: %v", render.ErrAsset, assetID, err)
	}
	if declared := baseMediaType(asset.MediaType); declared != "" && declared != baseMediaType(record.MediaType) {
		return nil, fmt.Errorf("%w: asset %q is declared %q but registered as %q",
			render.ErrAsset, assetID, asset.MediaType, record.MediaType)
	}
	body, err := r.store.Get(ctx, key, r.maxByte)
	if err != nil {
		return nil, transientOrAsset(err, assetID)
	}
	return newDigestReader(body, digest, assetID), nil
}

// ResolveRaw opens a bounded, digest-verified stream for the asset's raw
// variant. An asset with no raw object reports render.ErrAsset; a legacy
// companion without its own registered digest is rejected rather than
// verified against the clean digest.
func (r *S3AssetResolver) ResolveRaw(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	return r.ResolveVariant(ctx, assetID, asset, VariantRaw)
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
// resolved once, at first use: the registry record is read once and the
// variant it selects is fixed for the rest of the render, so a selector
// switch in another request can never mix bytes from before and after the
// switch inside one document. Downloads are shared across concurrent
// requests and the cache is bounded by entry count and total bytes; assets
// that do not fit the budget are streamed instead of cached.
func (r *S3AssetResolver) ForRender() render.AssetResolver {
	return &renderScope{base: r, cache: map[string][]byte{}, records: map[string]AssetRecord{},
		maxAssets: DefaultMaxCachedAssets, maxBytes: DefaultMaxCachedBytes}
}

var _ render.AssetResolver = (*renderScope)(nil)

// cacheKey identifies one immutable object within a render: the same asset
// ID, the selected variant and its registered digest. Two different digests
// for one ID are two cache entries, never one.
func cacheKey(record AssetRecord, variant Variant) string {
	key, digest, err := record.Select(variant)
	if err != nil {
		return record.AssetID + "\x00" + string(variant) + "\x00unavailable"
	}
	return record.AssetID + "\x00" + string(variant) + "\x00" + digest + "\x00" + key
}

type renderScope struct {
	base      *S3AssetResolver
	inflight  singleflight.Group
	maxAssets int
	maxBytes  int64
	mu        sync.Mutex
	cache     map[string][]byte
	records   map[string]AssetRecord
	used      int64
}

func (s *renderScope) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	return s.ResolveVariant(ctx, assetID, asset, VariantActive)
}

func (s *renderScope) ResolveVariant(ctx context.Context, assetID string, asset ir.Asset, variant Variant) (io.ReadCloser, error) {
	if err := ValidateAssetID(assetID); err != nil {
		return nil, fmt.Errorf("%w: %v", render.ErrAsset, err)
	}
	if asset.AssetID != assetID {
		return nil, fmt.Errorf("%w: asset %q was requested as %q", render.ErrAsset, asset.AssetID, assetID)
	}
	record, err := s.snapshot(ctx, assetID)
	if err != nil {
		return nil, err
	}
	key := cacheKey(record, variant)
	if data := s.peek(key); data != nil {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	value, err, _ := s.inflight.Do(key, func() (any, error) {
		if data := s.peek(key); data != nil {
			return data, nil
		}
		rc, err := s.base.open(ctx, record, assetID, asset, variant)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		s.admit(key, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(value.([]byte))), nil
}

// snapshot reads the registry record for assetID at most once per scope.
func (s *renderScope) snapshot(ctx context.Context, assetID string) (AssetRecord, error) {
	s.mu.Lock()
	record, ok := s.records[assetID]
	s.mu.Unlock()
	if ok {
		return record, nil
	}
	record, err := s.base.lookup.AssetRecord(ctx, assetID)
	switch {
	case errors.Is(err, ErrNotFound):
		return AssetRecord{}, fmt.Errorf("%w: asset %q is not registered", render.ErrAsset, assetID)
	case err != nil:
		return AssetRecord{}, transientOrAsset(err, assetID)
	case record.AssetID != assetID:
		return AssetRecord{}, fmt.Errorf("%w: asset lookup returned %q for %q", render.ErrAsset, record.AssetID, assetID)
	}
	s.mu.Lock()
	if existing, ok := s.records[assetID]; ok {
		// A concurrent request snapshotted first; its record wins, so every
		// read of this render sees one consistent selection.
		record = existing
	} else {
		s.records[assetID] = record
	}
	s.mu.Unlock()
	return record, nil
}

func (s *renderScope) peek(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[key]
}

// admit caches data when it fits both the entry and the byte budget.
func (s *renderScope) admit(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cache[key]; exists {
		return
	}
	if len(s.cache) >= s.maxAssets || s.used+int64(len(data)) > s.maxBytes {
		return
	}
	s.cache[key] = data
	s.used += int64(len(data))
}

// Cached returns the number of cached assets and bytes; tests use it.
func (s *renderScope) Cached() (assets int, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cache), s.used
}
