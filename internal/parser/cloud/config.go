// Package cloud implements parser.ParserAdapter against an OpenAI-compatible
// multimodal Chat Completions endpoint. It rasterizes pages locally, sends
// bounded JPEG transfer copies to a per-page extractor, merges page results
// deterministically (with an optional bounded text-only merge call), remaps
// model asset keys to server-generated IDs, validates and repairs the
// candidate IR, and only then crops and cleans the high-resolution pages.
// It never uploads objects or writes a database: the pipeline worker owns
// every side effect.
package cloud

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"time"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/vision/clean"
	"examforge/internal/vision/raster"
)

// Environment variable names for the live endpoint. The API key is only ever
// read here; it is never a flag, constant, fixture or log field.
const (
	EnvBaseURL = "EXAMFORGE_VLM_BASE_URL"
	EnvAPIKey  = "EXAMFORGE_VLM_API_KEY"
	EnvModel   = "EXAMFORGE_VLM_MODEL"
)

// Defaults for the bounded pipeline.
const (
	DefaultTransportLongEdge  = 2048
	DefaultJPEGQuality        = 80
	DefaultMaxJPEGBytes       = 2 << 20
	DefaultPageConcurrency    = 4
	DefaultPageRatePerSecond  = 2.0
	DefaultPageTimeout        = 90 * time.Second
	DefaultMergeTimeout       = 60 * time.Second
	DefaultRepairTimeout      = 60 * time.Second
	DefaultTotalParseTimeout  = 8 * time.Minute
	DefaultWorkerReserve      = 60 * time.Second
	DefaultMaxRepairRounds    = 2
	HardMaxRepairRounds       = 4
	DefaultMaxRequestBytes    = 8 << 20
	DefaultMaxResponseBytes   = 1 << 20
	DefaultMaxOutputTokens    = 8192
	DefaultMaxMergeInputBytes = 256 << 10
	DefaultMaxMergeTextBytes  = 96 << 10
	maxStageTimeout           = 10 * time.Minute
)

// Config configures a CloudAPIParser. Zero numeric fields select defaults;
// negative values are rejected.
type Config struct {
	// BaseURL, APIKey, Model configure the live endpoint. Tests inject an
	// httptest server URL and a placeholder key instead.
	BaseURL string
	APIKey  string
	Model   string

	// Rasterizer and Cleaner are the local image stages. Required.
	Rasterizer raster.Rasterizer
	Cleaner    clean.ImageCleaner
	// Validator runs the full IR schema and semantic validation. Required.
	Validator *ir.Validator
	// HTTPClient is used for every provider call. Nil builds a client with
	// Proxy disabled: no ambient proxy can route a request elsewhere.
	HTTPClient *http.Client
	// CropConfig bounds the final crop geometry.
	CropConfig raster.CropConfig

	TransportLongEdge int
	JPEGQuality       int
	MaxJPEGBytes      int
	PageConcurrency   int
	PageRatePerSecond float64
	PageTimeout       time.Duration
	MergeTimeout      time.Duration
	RepairTimeout     time.Duration
	RasterTimeout     time.Duration
	TotalParseTimeout time.Duration
	// WorkerReserve is the budget kept after parsing for the worker's
	// uploads, persistence and error bookkeeping.
	WorkerReserve time.Duration
	// JobTimeout is the River ingest job timeout the budget must fit inside:
	// TotalParseTimeout + WorkerReserve must be strictly less. Zero skips the
	// check (the caller asserts it elsewhere).
	JobTimeout time.Duration

	MaxRepairRounds    int
	MaxRequestBytes    int
	MaxResponseBytes   int
	MaxOutputTokens    int
	MaxMergeInputBytes int
	MaxMergeTextBytes  int

	// Extractor and Merger override the OpenAI-compatible protocol seams;
	// nil builds them over the shared HTTP transport. Tests inject fakes.
	Extractor PageExtractor
	Merger    TextMerger

	// NewAssetID generates a server asset ID (ast_...). Nil uses ast_ + UUID.
	NewAssetID func() string
	// NewExamID generates the exam_id. Nil uses exam_ + UUID.
	NewExamID func() string
	// Diagnostics receives one bounded summary per parse. Nil discards.
	Diagnostics func(Diagnostics)
}

// Diagnostics is the bounded per-parse observability record. It never carries
// image bytes, base64, secrets, full IR or provider response bodies.
type Diagnostics struct {
	Pages            int
	RasterMillis     int64
	ExtractMillis    int64
	MergeMillis      int64
	RepairMillis     int64
	CropMillis       int64
	RepairRounds     int
	MergeCalls       int
	Warnings         int
	PromptTokens     int
	CompletionTokens int
}

// EnvConfig reads the live endpoint configuration from the environment. All
// three variables must be set; nothing is defaulted to a vendor domain.
func EnvConfig() (baseURL, apiKey, model string, err error) {
	baseURL, apiKey, model = os.Getenv(EnvBaseURL), os.Getenv(EnvAPIKey), os.Getenv(EnvModel)
	var missing []string
	if baseURL == "" {
		missing = append(missing, EnvBaseURL)
	}
	if apiKey == "" {
		missing = append(missing, EnvAPIKey)
	}
	if model == "" {
		missing = append(missing, EnvModel)
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("cloud: missing environment variables: %v", missing)
	}
	return baseURL, apiKey, model, nil
}

func (c *Config) applyDefaults() error {
	if c.Rasterizer == nil || c.Cleaner == nil || c.Validator == nil {
		return errors.New("cloud: config needs a rasterizer, cleaner and validator")
	}
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return errors.New("cloud: config needs BaseURL, APIKey and Model")
	}
	negInts := []int{c.TransportLongEdge, c.JPEGQuality, c.MaxJPEGBytes, c.PageConcurrency,
		c.MaxRepairRounds, c.MaxRequestBytes, c.MaxResponseBytes, c.MaxOutputTokens,
		c.MaxMergeInputBytes, c.MaxMergeTextBytes}
	for _, v := range negInts {
		if v < 0 {
			return errors.New("cloud: config limits must not be negative")
		}
	}
	if c.PageRatePerSecond < 0 || math.IsNaN(c.PageRatePerSecond) || math.IsInf(c.PageRatePerSecond, 0) {
		return errors.New("cloud: page rate must be finite and not negative")
	}
	for _, d := range []time.Duration{c.PageTimeout, c.MergeTimeout, c.RepairTimeout, c.RasterTimeout,
		c.TotalParseTimeout, c.WorkerReserve, c.JobTimeout} {
		if d < 0 {
			return errors.New("cloud: timeouts must not be negative")
		}
	}
	setInt(&c.TransportLongEdge, DefaultTransportLongEdge)
	setInt(&c.JPEGQuality, DefaultJPEGQuality)
	setInt(&c.MaxJPEGBytes, DefaultMaxJPEGBytes)
	setInt(&c.PageConcurrency, DefaultPageConcurrency)
	setInt(&c.MaxRepairRounds, DefaultMaxRepairRounds)
	setInt(&c.MaxRequestBytes, DefaultMaxRequestBytes)
	setInt(&c.MaxResponseBytes, DefaultMaxResponseBytes)
	setInt(&c.MaxOutputTokens, DefaultMaxOutputTokens)
	setInt(&c.MaxMergeInputBytes, DefaultMaxMergeInputBytes)
	setInt(&c.MaxMergeTextBytes, DefaultMaxMergeTextBytes)
	if c.JPEGQuality < 1 || c.JPEGQuality > 100 {
		return errors.New("cloud: JPEG quality must be 1..100")
	}
	if c.MaxRepairRounds > HardMaxRepairRounds {
		return fmt.Errorf("cloud: repair rounds %d exceed the hard ceiling %d", c.MaxRepairRounds, HardMaxRepairRounds)
	}
	if c.PageRatePerSecond == 0 {
		c.PageRatePerSecond = DefaultPageRatePerSecond
	}
	setDur(&c.PageTimeout, DefaultPageTimeout)
	setDur(&c.MergeTimeout, DefaultMergeTimeout)
	setDur(&c.RepairTimeout, DefaultRepairTimeout)
	setDur(&c.RasterTimeout, raster.DefaultProcessTimeout)
	setDur(&c.TotalParseTimeout, DefaultTotalParseTimeout)
	setDur(&c.WorkerReserve, DefaultWorkerReserve)
	for _, d := range []time.Duration{c.PageTimeout, c.MergeTimeout, c.RepairTimeout, c.RasterTimeout} {
		if d > maxStageTimeout {
			return fmt.Errorf("cloud: stage timeout %v exceeds %v", d, maxStageTimeout)
		}
	}
	// The total budget must leave the worker reserve inside the job timeout.
	if c.JobTimeout > 0 && c.TotalParseTimeout+c.WorkerReserve >= c.JobTimeout {
		return fmt.Errorf("cloud: total_parse_timeout %v + worker reserve %v must stay strictly below the job timeout %v",
			c.TotalParseTimeout, c.WorkerReserve, c.JobTimeout)
	}
	if c.TotalParseTimeout <= c.WorkerReserve {
		return errors.New("cloud: total parse budget must exceed the worker reserve")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: c.PageConcurrency},
		}
	}
	if c.NewAssetID == nil {
		c.NewAssetID = defaultAssetID
	}
	if c.NewExamID == nil {
		c.NewExamID = defaultExamID
	}
	return nil
}

func setInt(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

func setDur(p *time.Duration, def time.Duration) {
	if *p == 0 {
		*p = def
	}
}

var _ parser.ParserAdapter = (*CloudAPIParser)(nil)
