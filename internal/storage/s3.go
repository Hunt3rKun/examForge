package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Config configures S3Store. Credentials are injected by the operator at
// run time (environment or secret manager) and never appear in constants,
// flags, logs or fixtures.
type S3Config struct {
	// Endpoint is the S3 protocol base URL (scheme, host, optional port).
	Endpoint string
	Region   string
	Bucket   string
	// AccessKeyID and SecretAccessKey are the test or service credentials.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// ForcePathStyle must be true. Bucket-in-host virtual hosting is
	// rejected because many on-premise S3 services do not support it.
	ForcePathStyle bool
	// MaxObjectBytes bounds one upload. Default: 64 MiB.
	MaxObjectBytes int64
	// RequestTimeout bounds one S3 request. Default: 30s.
	RequestTimeout time.Duration
	// HTTPClient is a test hook; production leaves it nil.
	HTTPClient *http.Client
}

// S3Store talks the S3 protocol with path-style addressing forced on.
type S3Store struct {
	client   *s3.Client
	bucket   string
	maxBytes int64
	timeout  time.Duration
}

var _ ObjectStore = (*S3Store)(nil)
var _ ObjectLister = (*S3Store)(nil)

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// NewS3Store validates the configuration and builds the client. It performs
// no network request.
func NewS3Store(in S3Config) (*S3Store, error) {
	u, err := url.Parse(in.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%w: endpoint must be an http(s) URL without credentials, path or query", ErrInvalid)
	}
	if strings.TrimSpace(in.Region) == "" {
		return nil, fmt.Errorf("%w: region is required", ErrInvalid)
	}
	if !bucketName.MatchString(in.Bucket) {
		return nil, fmt.Errorf("%w: bucket name is not valid", ErrInvalid)
	}
	if in.AccessKeyID == "" || in.SecretAccessKey == "" {
		return nil, fmt.Errorf("%w: credentials are required", ErrInvalid)
	}
	if !in.ForcePathStyle {
		return nil, fmt.Errorf("%w: ForcePathStyle must be true; virtual-hosted addressing is not allowed", ErrInvalid)
	}
	if in.MaxObjectBytes < 0 || in.RequestTimeout < 0 {
		return nil, fmt.Errorf("%w: numeric limits must not be negative", ErrInvalid)
	}
	store := &S3Store{
		bucket:   in.Bucket,
		maxBytes: in.MaxObjectBytes,
		timeout:  in.RequestTimeout,
	}
	region := in.Region
	creds := credentials.NewStaticCredentialsProvider(in.AccessKeyID, in.SecretAccessKey, in.SessionToken)
	if store.maxBytes == 0 {
		store.maxBytes = MaxPutBytes
	}
	if store.timeout == 0 {
		store.timeout = 30 * time.Second
	}
	// Build aws.Config field by field instead of LoadDefaultConfig: the
	// process environment, shared config and instance metadata are never read,
	// so credentials can only come from this configuration.
	// A private client: the process proxy environment is not consulted, so a
	// proxy variable cannot redirect object traffic or read credentials.
	httpClient := aws.HTTPClient(privateHTTPClient())
	if in.HTTPClient != nil {
		httpClient = in.HTTPClient
	}
	cfg := aws.Config{
		Region:       region,
		Credentials:  creds,
		HTTPClient:   httpClient,
		DefaultsMode: aws.DefaultsModeStandard,
	}
	store.client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(strings.TrimSuffix(in.Endpoint, "/"))
		o.UsePathStyle = true // forced: never override with virtual-hosted style
	})
	return store, nil
}

// privateHTTPClient builds the default transport: no environment proxy,
// bounded connection pool and dial/TLS timeouts.
func privateHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// CreateBucket creates the bucket when it does not exist. Integration tests
// use it for per-test isolation; production provisioners own this step.
func (s *S3Store) CreateBucket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil && !isCreateAlreadyOwned(err) {
		return classify(err, "bucket:"+s.bucket)
	}
	return nil
}

func isCreateAlreadyOwned(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode() == "BucketAlreadyOwnedByYou" || apiErr.ErrorCode() == "BucketAlreadyExists")
}

func (s *S3Store) create(ctx context.Context, key string, data []byte, sha, contentType string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
		Metadata:      map[string]string{HashMetaKey: sha},
	})
	if err != nil {
		return classify(err, key)
	}
	// S3 has no portable conditional create. The post-create digest check
	// catches a concurrent writer whose content replaced ours; the resolver's
	// stream hash check covers the remaining window at read time.
	info, err := s.Stat(ctx, key)
	if err != nil {
		return err
	}
	if got := info.Metadata[HashMetaKey]; got != "" && got != sha {
		return fmt.Errorf("%w: key %s was written concurrently with different content", ErrConflict, key)
	}
	return nil
}

// Put stores exactly size bytes. An existing key is verified by digest:
// equal content is idempotent, different content is ErrConflict and the
// stored object is not replaced.
func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := putImmutable(ctx, s, s, key, body, size, contentType, s.maxBytes)
	return err
}

func (s *S3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectInfo{}, classify(err, key)
	}
	info := ObjectInfo{Key: key, ContentType: aws.ToString(out.ContentType), Metadata: out.Metadata}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ETag != nil {
		info.ETag = *out.ETag
	}
	return info, nil
}

// Get returns a stream that fails with ErrTooLarge once more than maxBytes
// have been read. A too-large object is rejected by its HEAD size before any
// body is downloaded.
func (s *S3Store) Get(ctx context.Context, key string, maxBytes int64) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be positive")
	}
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if info.Size > maxBytes {
		return nil, tooLarge(key, info.Size, maxBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		cancel()
		return nil, classify(err, key)
	}
	if out.ContentLength != nil && *out.ContentLength > maxBytes {
		out.Body.Close()
		cancel()
		return nil, tooLarge(key, *out.ContentLength, maxBytes)
	}
	return &cancelReader{ReadCloser: newBoundedReader(out.Body, maxBytes), cancel: cancel}, nil
}

// Delete removes one key. S3 reports success for a missing key too.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return classify(err, key)
	}
	return nil
}

// List enumerates the keys under prefix. ContentType and Metadata are empty
// in a listing; call Stat for them.
func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	prefix = strings.TrimSuffix(prefix, "/")
	var (
		out   []ObjectInfo
		token *string
	)
	for {
		listCtx, cancel := context.WithTimeout(ctx, s.timeout)
		page, err := s.client.ListObjectsV2(listCtx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		cancel()
		if err != nil {
			return nil, classify(err, prefix)
		}
		for _, obj := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key)}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.ETag != nil {
				info.ETag = *obj.ETag
			}
			out = append(out, info)
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// DeletePrefix removes every object under prefix and returns the keys removed.
// Use it for orphaned uploads and failed ingest jobs.
func DeletePrefix(ctx context.Context, store ObjectStore, prefix string) ([]string, error) {
	lister, ok := store.(ObjectLister)
	if !ok {
		return nil, errors.New("object store cannot list")
	}
	keys, err := lister.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(keys))
	for _, obj := range keys {
		if err := store.Delete(ctx, obj.Key); err != nil {
			return removed, err
		}
		removed = append(removed, obj.Key)
	}
	return removed, nil
}

// cancelReader cancels the request context after the body is closed so the
// Get deadline does not outlive the stream.
type cancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelReader) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

// classify maps an SDK error to a storage error class without leaking the
// endpoint URL, credentials or response body.
func classify(err error, key string) error {
	var (
		apiErr  smithy.APIError
		httpErr *awshttp.ResponseError
	)
	code, status := "", 0
	if errors.As(err, &apiErr) {
		code = apiErr.ErrorCode()
	}
	if errors.As(err, &httpErr) && httpErr.HTTPResponse() != nil {
		status = httpErr.HTTPResponse().StatusCode
	}
	detail := fmt.Sprintf("key %s: status %d, code %s", key, status, code)
	switch {
	case status == 404 || code == "NoSuchKey" || code == "NotFound":
		return fmt.Errorf("%w (%s)", ErrNotFound, detail)
	case status == 412 || code == "PreconditionFailed":
		return fmt.Errorf("%w (%s)", errAlreadyExists, detail)
	case status == 403 || code == "AccessDenied" || code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch" ||
		code == "AllAccessDisabled" || code == "NoSuchBucket" || code == "AuthorizationHeaderMalformed":
		return fmt.Errorf("%w (%s)", ErrForbidden, detail)
	case status == 301 || code == "PermanentRedirect":
		// The endpoint wants virtual-hosted addressing; path-style is forced.
		return fmt.Errorf("%w (%s)", ErrForbidden, detail)
	case status == 400 || code == "InvalidArgument" || code == "InvalidObjectName" || code == "MethodNotAllowed":
		// A malformed request never succeeds on retry.
		return fmt.Errorf("%w (%s)", ErrInvalidKey, detail)
	}
	return fmt.Errorf("%w (%s)", ErrUnavailable, detail)
}
