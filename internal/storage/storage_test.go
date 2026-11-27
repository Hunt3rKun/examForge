package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateKeyRejectsUnsafeKeys(t *testing.T) {
	bad := map[string]string{
		"empty":            "",
		"absolute":         "/assets/raw/a.png",
		"empty segment":    "assets//raw/a.png",
		"dot segment":      "assets/./a.png",
		"dotdot segment":   "assets/../a.png",
		"traversal prefix": "../assets/a.png",
		"backslash":        `assets\raw\a.png`,
		"nul":              "assets/a\x00.png",
		"control":          "assets/a\n.png",
		"percent encoded":  "assets/%2e%2e/a.png",
		"encoded slash":    "assets/a%2Fb.png",
		"trailing slash":   "assets/raw/",
		"leading dot":      ".hidden/a.png",
		"too long":         strings.Repeat("a", maxKeyBytes+1),
		"space":            "assets/raw/a b.png",
	}
	for name, key := range bad {
		if err := ValidateKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: ValidateKey(%q) = %v, want ErrInvalidKey", name, key, err)
		}
	}
	for _, key := range []string{"assets/raw/ast_1.png", "uploads/" + uuid.NewString() + "/source.pdf", "a"} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestKeyBuildersAreDeterministicAndValidated(t *testing.T) {
	id := "ast_01jexam0000000000000000001"
	job := uuid.New()
	if got, want := SourceKey(job), "uploads/"+job.String()+"/source.pdf"; got != want {
		t.Errorf("SourceKey = %q, want %q", got, want)
	}
	raw, err := RawAssetKey(id, "image/png")
	if err != nil || raw != "assets/raw/"+id+".png" {
		t.Fatalf("RawAssetKey = %q, %v", raw, err)
	}
	clean, err := CleanAssetKey(id, "image/svg+xml; charset=binary")
	if err != nil || clean != "assets/clean/"+id+".svg" {
		t.Fatalf("CleanAssetKey = %q, %v", clean, err)
	}
	if _, err := RawAssetKey("../../etc/passwd", "image/png"); !errors.Is(err, ErrInvalid) {
		t.Errorf("asset ID with path segments: err = %v, want ErrInvalid", err)
	}
	if _, err := RawAssetKey(id, "text/html"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unsupported media type: err = %v, want ErrInvalid", err)
	}
	// Every generated key passes the validator.
	for _, key := range []string{SourceKey(job), raw, clean, JobKeyPrefix(job) + "source.pdf"} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("generated key %q rejected: %v", key, err)
		}
	}
}

func TestMemStorePutIsImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	key := "assets/raw/ast_1.png"
	sum, err := putOf(ctx, store, key, []byte("first"))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if sum != HashBytes([]byte("first")) {
		t.Errorf("digest = %s", sum)
	}
	// Same content: idempotent, no second create.
	if _, err := putOf(ctx, store, key, []byte("first")); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	if got := store.CreateAttempts(key); got != 1 {
		t.Errorf("create attempts = %d, want 1", got)
	}
	// Different content: rejected, original untouched.
	if _, err := putOf(ctx, store, key, []byte("second")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting put err = %v, want ErrConflict", err)
	}
	got := readAll(t, ctx, store, key, 1<<20)
	if string(got) != "first" {
		t.Errorf("object was replaced: %q", got)
	}
	// An object written without metadata is verified by its bytes.
	bare := "assets/raw/ast_bare.png"
	if err := store.create(ctx, bare, []byte("bare"), "", "image/png"); err != nil {
		t.Fatalf("seed bare object: %v", err)
	}
	if _, err := putOf(ctx, store, bare, []byte("bare")); err != nil {
		t.Errorf("idempotent put without metadata: %v", err)
	}
	if _, err := putOf(ctx, store, bare, []byte("other")); !errors.Is(err, ErrConflict) {
		t.Errorf("conflict without metadata: err = %v, want ErrConflict", err)
	}
}

func TestPutRejectsBadSizeTypeAndKey(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	cases := map[string]struct {
		key  string
		body string
		size int64
		typ  string
	}{
		"negative size":  {"a/b.png", "x", -1, "image/png"},
		"size too large": {"a/b.png", "x", MaxPutBytes + 1, "image/png"},
		"over limit":     {"a/b.png", "x", 11, "image/png"},
		"short body":     {"a/b.png", "x", 3, "image/png"},
		"long body":      {"a/b.png", "xyz", 2, "image/png"},
		"no type":        {"a/b.png", "x", 1, ""},
		"bad type":       {"a/b.png", "x", 1, "image/png\r\nX-Injected: 1"},
		"bad key":        {"../b.png", "x", 1, "image/png"},
	}
	for name, c := range cases {
		_, err := put(ctx, store, c.key, strings.NewReader(c.body), c.size, c.typ, 10)
		var want error = ErrInvalid
		switch name {
		case "size too large", "over limit":
			want = ErrTooLarge
		case "bad key":
			want = ErrInvalidKey
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
	}
	if len(store.objects) != 0 {
		t.Errorf("rejected puts stored %d objects", len(store.objects))
	}
}

func putOf(ctx context.Context, store ObjectStore, key string, data []byte) (string, error) {
	err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "image/png")
	return HashBytes(data), err
}

func put(ctx context.Context, store ObjectStore, key string, body io.Reader, size int64, typ string, limit int64) (string, error) {
	cr, ok := store.(creator)
	if !ok {
		return "", errors.New("store is not a creator")
	}
	return putImmutable(ctx, store, cr, key, body, size, typ, limit)
}

func readAll(t *testing.T, ctx context.Context, store ObjectStore, key string, max int64) []byte {
	t.Helper()
	rc, err := store.Get(ctx, key, max)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data
}

func TestGetBoundedReaderRejectsOversizedObjects(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	if _, err := putOf(ctx, store, "a/big.png", bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	// Larger than the bound: rejected by HEAD size before any body.
	if _, err := store.Get(ctx, "a/big.png", 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Get with small bound err = %v, want ErrTooLarge", err)
	}
	// Within the bound by size, a stream that delivers more than maxBytes
	// fails at the limit+1 byte.
	rc, err := store.Get(ctx, "a/big.png", 1000)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, rc)
	rc.Close()
	if err != nil || n != 100 {
		t.Errorf("read %d bytes, err %v", n, err)
	}
	if _, err := store.Get(ctx, "a/missing.png", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key err = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "../escape", 10); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("invalid key err = %v, want ErrInvalidKey", err)
	}
	if _, err := store.Get(ctx, "a/big.png", 0); err == nil {
		t.Error("maxBytes 0 accepted")
	}
}

func TestMemStoreFaultsAreClassifiedByCaller(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	if _, err := putOf(ctx, store, "a/b.png", []byte("x")); err != nil {
		t.Fatal(err)
	}
	store.SetFault(1, fmt.Errorf("%w: dial timeout", ErrUnavailable))
	if _, err := store.Stat(ctx, "a/b.png"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("fault err = %v, want ErrUnavailable", err)
	}
	if transient, _ := Classify(fmt.Errorf("%w: x", ErrUnavailable)); !transient {
		t.Error("ErrUnavailable should classify transient")
	}
	if _, err := store.Stat(ctx, "a/b.png"); err != nil {
		t.Errorf("fault did not clear: %v", err)
	}
	if _, nf := Classify(ErrNotFound); !nf {
		t.Error("ErrNotFound should classify not-found")
	}
}

func TestGetHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := NewMemStore()
	cancel()
	if _, err := store.Get(ctx, "a/b.png", 10); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Get err = %v, want context.Canceled", err)
	}
	if _, err := putOf(ctx, store, "a/c.png", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Put err = %v, want context.Canceled", err)
	}
}

// s3stub is a minimal S3-protocol server. It records every request so tests
// assert path-style addressing and injects responses per method.
type s3stub struct {
	mu       sync.Mutex
	objects  map[string]stubObject
	requests []string // "METHOD host /path"
	host     string
	status   map[string]int
	hang     map[string]time.Duration
}

type stubObject struct {
	data []byte
	typ  string
	sha  string
}

func (s *s3stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.Host+" "+r.URL.Path)
	status, hang := s.status[r.Method], s.hang[r.Method]
	s.mu.Unlock()
	if hang > 0 {
		select {
		case <-time.After(hang):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 && status != http.StatusOK {
		s3Error(w, status, codeFor(status))
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "examforge" {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.objects[key] = stubObject{data: body, typ: r.Header.Get("Content-Type"), sha: r.Header.Get("x-amz-meta-sha256")}
		s.mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
	case http.MethodHead:
		s.mu.Lock()
		obj, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(obj.data)))
		w.Header().Set("Content-Type", obj.typ)
		if obj.sha != "" {
			w.Header().Set("x-amz-meta-sha256", obj.sha)
		}
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			s.list(w, r.URL.Query().Get("prefix"))
			return
		}
		s.mu.Lock()
		obj, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(obj.data)))
		w.Header().Set("Content-Type", obj.typ)
		_, _ = w.Write(obj.data)
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (s *s3stub) list(w http.ResponseWriter, prefix string) {
	type object struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	s.mu.Lock()
	var contents []object
	for key, obj := range s.objects {
		if strings.HasPrefix(key, prefix) {
			contents = append(contents, object{key, len(obj.data)})
		}
	}
	s.mu.Unlock()
	type result struct {
		XMLName xml.Name `xml:"ListBucketResult"`
		IsTrunc bool     `xml:"IsTruncated"`
		Content []object `xml:"Contents"`
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result{Content: contents})
}

func s3Error(w http.ResponseWriter, status int, code string) {
	type detail struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(detail{Code: code, Message: code})
}

func codeFor(status int) string {
	switch status {
	case http.StatusNotFound:
		return "NoSuchKey"
	case http.StatusForbidden:
		return "AccessDenied"
	case http.StatusPreconditionFailed:
		return "PreconditionFailed"
	case http.StatusBadRequest:
		return "InvalidArgument"
	}
	return "InternalError"
}

func newStubStore(t *testing.T, stub *s3stub) *S3Store {
	t.Helper()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	stub.host = strings.TrimPrefix(server.URL, "http://")
	store, err := NewS3Store(S3Config{
		Endpoint:        server.URL,
		Region:          "us-east-1",
		Bucket:          "examforge",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		ForcePathStyle:  true,
		RequestTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store
}

func TestNewS3StoreRequiresPathStyleAndCompleteConfig(t *testing.T) {
	valid := S3Config{Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "examforge",
		AccessKeyID: "k", SecretAccessKey: "s", ForcePathStyle: true}
	if _, err := NewS3Store(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := map[string]func(*S3Config){
		"virtual host style":  func(c *S3Config) { c.ForcePathStyle = false },
		"no endpoint":         func(c *S3Config) { c.Endpoint = "" },
		"endpoint with creds": func(c *S3Config) { c.Endpoint = "http://ak:sk@host:9000" },
		"endpoint with path":  func(c *S3Config) { c.Endpoint = "http://host:9000/prefix" },
		"endpoint with query": func(c *S3Config) { c.Endpoint = "http://host:9000?x=1" },
		"non-http scheme":     func(c *S3Config) { c.Endpoint = "ftp://host" },
		"no region":           func(c *S3Config) { c.Region = "" },
		"no bucket":           func(c *S3Config) { c.Bucket = "" },
		"bad bucket":          func(c *S3Config) { c.Bucket = "Bucket" },
		"no secret":           func(c *S3Config) { c.SecretAccessKey = "" },
		"no access key":       func(c *S3Config) { c.AccessKeyID = "" },
		"negative limit":      func(c *S3Config) { c.MaxObjectBytes = -1 },
	}
	for name, mutate := range bad {
		cfg := valid
		mutate(&cfg)
		if _, err := NewS3Store(cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestS3StoreUsesPathStyleAndClassifiesErrors(t *testing.T) {
	ctx := context.Background()
	stub := &s3stub{objects: map[string]stubObject{}}
	store := newStubStore(t, stub)
	key := "assets/raw/ast_1.png"
	if _, err := putOf(ctx, store, key, []byte("png-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := putOf(ctx, store, key, []byte("png-bytes")); err != nil {
		t.Fatalf("idempotent Put: %v", err)
	}
	if _, err := putOf(ctx, store, key, []byte("other")); !errors.Is(err, ErrConflict) {
		t.Errorf("conflicting Put err = %v, want ErrConflict", err)
	}
	if got := readAll(t, ctx, store, key, 100); string(got) != "png-bytes" {
		t.Errorf("object content = %q after a rejected conflict", got)
	}
	info, err := store.Stat(ctx, key)
	if err != nil || info.Size != 9 || info.Metadata[HashMetaKey] != HashBytes([]byte("png-bytes")) {
		t.Errorf("Stat = %+v, %v", info, err)
	}

	for _, r := range stub.requests {
		method, rest, _ := strings.Cut(r, " ")
		host, path, _ := strings.Cut(rest, " ")
		if strings.Contains(host, "examforge") || !strings.HasPrefix(path, "/examforge/") {
			t.Errorf("request is not path-style: %s %s%s", method, host, path)
		}
	}
	for _, want := range []string{"PUT", "HEAD", "GET"} {
		found := false
		for _, r := range stub.requests {
			found = found || r == want+" "+stub.host+" /examforge/"+key
		}
		if !found {
			t.Errorf("no %s of /examforge/%s in %v", want, key, stub.requests)
		}
	}

	cases := []struct {
		status      int
		want        error
		description string
	}{
		{http.StatusNotFound, ErrNotFound, "missing object"},
		{http.StatusForbidden, ErrForbidden, "access denied"},
		{http.StatusServiceUnavailable, ErrUnavailable, "service unavailable"},
		{http.StatusBadRequest, ErrInvalidKey, "invalid key"},
	}
	for _, c := range cases {
		stub.status = map[string]int{http.MethodHead: c.status}
		_, err := store.Stat(ctx, key)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: Stat err = %v, want %v", c.description, err, c.want)
		}
		// The message must not leak the endpoint, credentials or response body.
		if err != nil {
			msg := err.Error()
			for _, secret := range []string{"127.0.0.1", "test-key", "test-secret", "<Error>"} {
				if strings.Contains(msg, secret) {
					t.Errorf("%s: error leaks %q: %s", c.description, secret, msg)
				}
			}
		}
	}
	stub.status = nil
}

func TestS3StoreTimeoutsAreTransientAndBounded(t *testing.T) {
	ctx := context.Background()
	stub := &s3stub{objects: map[string]stubObject{}, hang: map[string]time.Duration{http.MethodHead: 2 * time.Second}}
	store := newStubStore(t, stub)
	store.timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := store.Stat(ctx, "a/b.png"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("timeout err = %v, want ErrUnavailable", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("request did not honour the timeout: %s", time.Since(start))
	}
}

func TestS3StoreGetRejectsOversizedAndLists(t *testing.T) {
	ctx := context.Background()
	stub := &s3stub{objects: map[string]stubObject{}}
	store := newStubStore(t, stub)
	for _, key := range []string{"assets/raw/ast_1.png", "assets/raw/ast_2.png", "assets/clean/ast_1.png"} {
		if _, err := putOf(ctx, store, key, bytes.Repeat([]byte("z"), 500)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Get(ctx, "assets/raw/ast_1.png", 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Get err = %v, want ErrTooLarge", err)
	}
	if got := readAll(t, ctx, store, "assets/raw/ast_1.png", 500); len(got) != 500 {
		t.Errorf("read %d bytes", len(got))
	}
	keys, err := DeletePrefix(ctx, store, "assets/raw/")
	if err != nil || len(keys) != 2 {
		t.Fatalf("DeletePrefix = %v, %v", keys, err)
	}
	left, err := store.List(ctx, "assets/")
	if err != nil || len(left) != 1 || left[0].Key != "assets/clean/ast_1.png" {
		t.Errorf("List after delete = %+v, %v", left, err)
	}
}
