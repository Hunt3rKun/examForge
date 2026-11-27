package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// MemStore is an in-memory ObjectStore for offline tests. It applies the same
// key, size and immutability rules as S3Store; it never touches the network.
// Fault injection lets tests exercise transient and permanent store failures.
type MemStore struct {
	mu      sync.Mutex
	objects map[string]memObject
	// FailNext makes the next n store operations fail with the error set by
	// SetFault, after which the store works again.
	failNext int
	fault    error
	puts     map[string]int // create attempts per key
}

type memObject struct {
	data        []byte
	contentType string
	sha         string
}

var _ ObjectStore = (*MemStore)(nil)
var _ ObjectLister = (*MemStore)(nil)

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{objects: map[string]memObject{}, puts: map[string]int{}}
}

// SetFault makes the next n operations fail with err. n = 0 clears the fault.
func (m *MemStore) SetFault(n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext, m.fault = n, err
}

func sortStrings(s []string) { sort.Strings(s) }

func (m *MemStore) trip() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext <= 0 {
		return nil
	}
	m.failNext--
	return m.fault
}

// CreateAttempts reports how many create attempts a key has seen. It lets a
// test assert that an idempotent put did not re-upload.
func (m *MemStore) CreateAttempts(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts[key]
}

func (m *MemStore) create(ctx context.Context, key string, data []byte, sha, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.trip(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts[key]++
	if _, exists := m.objects[key]; exists {
		return errAlreadyExists
	}
	m.objects[key] = memObject{data: bytes.Clone(data), contentType: contentType, sha: sha}
	return nil
}

func (m *MemStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := putImmutable(ctx, m, m, key, body, size, contentType, MaxPutBytes)
	return err
}

func (m *MemStore) Get(ctx context.Context, key string, maxBytes int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.trip(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	obj, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		return nil, notFound(key)
	}
	if int64(len(obj.data)) > maxBytes {
		return nil, tooLarge(key, int64(len(obj.data)), maxBytes)
	}
	return newBoundedReader(io.NopCloser(bytes.NewReader(obj.data)), maxBytes), nil
}

func (m *MemStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if err := m.trip(); err != nil {
		return ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, notFound(key)
	}
	return ObjectInfo{Key: key, Size: int64(len(obj.data)), ContentType: obj.contentType,
		ETag: obj.sha, Metadata: map[string]string{HashMetaKey: obj.sha}}, nil
}

func (m *MemStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.trip(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectInfo
	for key, obj := range m.objects {
		if len(prefix) <= len(key) && key[:len(prefix)] == prefix {
			out = append(out, ObjectInfo{Key: key, Size: int64(len(obj.data)), ETag: obj.sha})
		}
	}
	return out, nil
}

// Keys returns every stored key, sorted, for assertions.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}

func notFound(key string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, key)
}

func tooLarge(key string, size, max int64) error {
	return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrTooLarge, key, size, max)
}
