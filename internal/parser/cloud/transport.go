package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"examforge/internal/parser"
	"examforge/internal/storage"
)

// chatRequest is the OpenAI-compatible Chat Completions request body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// chatResponse is the bounded subset of the response this client reads.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// transport is the shared HTTP protocol layer: one token bucket and one
// bounded request/response path for page extraction, text merge and repair.
type transport struct {
	cfg     Config
	client  *http.Client
	limiter *rate.Limiter
}

func newTransport(cfg Config) *transport {
	burst := cfg.PageConcurrency
	if burst < 1 {
		burst = 1
	}
	return &transport{
		cfg:     cfg,
		client:  cfg.HTTPClient,
		limiter: rate.NewLimiter(rate.Limit(cfg.PageRatePerSecond), burst),
	}
}

// usage accumulates provider token usage across every call of one parse.
// Page extractions run concurrently, so the counters are atomic.
type usage struct {
	prompt     atomic.Int64
	completion atomic.Int64
}

func (u *usage) add(prompt, completion int) {
	u.prompt.Add(int64(prompt))
	u.completion.Add(int64(completion))
}

// call posts one chat completion and decodes the bounded response. HTTP
// error classes follow the specification table: 400/401/403 permanent,
// 429/5xx/network/timeout transient. Response bodies never enter errors or
// logs beyond a short redacted snippet.
func (t *transport) call(ctx context.Context, req chatRequest, timeout time.Duration, u *usage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := t.limiter.Wait(ctx); err != nil {
		return "", parser.Transient("vlm_rate_cancelled", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", parser.Permanent("vlm_request_invalid", err)
	}
	if len(body) > t.cfg.MaxRequestBytes {
		return "", parser.Permanent("vlm_request_too_large", fmt.Errorf("request is %d bytes; limit %d", len(body), t.cfg.MaxRequestBytes))
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		strings.TrimRight(t.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", parser.Permanent("vlm_request_invalid", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Network, DNS and timeout failures may succeed on another attempt.
		return "", parser.Transient("vlm_unavailable", fmt.Errorf("provider request failed: %v", sanitize(err)))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.cfg.MaxResponseBytes)+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", parser.Transient("vlm_unavailable", fmt.Errorf("read provider response: %v", sanitize(err)))
	}
	if len(data) > t.cfg.MaxResponseBytes {
		return "", parser.Permanent("vlm_response_too_large", fmt.Errorf("response exceeds %d bytes", t.cfg.MaxResponseBytes))
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", parser.Transient("vlm_rate_limited", fmt.Errorf("provider returned HTTP 429"))
	case resp.StatusCode >= 500:
		return "", parser.Transient("vlm_unavailable", fmt.Errorf("provider returned HTTP %d", resp.StatusCode))
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", parser.Permanent("vlm_rejected", fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, snippet(data)))
	case resp.StatusCode != http.StatusOK:
		return "", parser.Permanent("vlm_protocol", fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, snippet(data)))
	}
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", parser.Permanent("vlm_protocol", fmt.Errorf("response is not the expected JSON structure: %s", snippet(data)))
	}
	u.add(out.Usage.PromptTokens, out.Usage.CompletionTokens)
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", parser.Permanent("vlm_protocol", fmt.Errorf("response carries no message content: %s", snippet(data)))
	}
	return out.Choices[0].Message.Content, nil
}

// sanitize strips URLs and the API key from a transport error message.
func sanitize(err error) error {
	return fmt.Errorf("%s", storage.Redact(err.Error(), 256))
}

// snippet returns a bounded, redacted response fragment for error context.
func snippet(data []byte) string {
	return storage.Redact(string(data), 200)
}

// extractJSON pulls the first JSON object out of a model reply, tolerating a
// fenced code block. It never evaluates anything.
func extractJSON(content string) ([]byte, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("reply contains no JSON object")
	}
	candidate := []byte(s[start : end+1])
	if !json.Valid(candidate) {
		return nil, fmt.Errorf("reply JSON is malformed")
	}
	return candidate, nil
}
