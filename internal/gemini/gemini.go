// Package gemini is a small client for Google's Gemini generateContent API.
//
// It deliberately avoids the official SDK: the task manager only needs a
// single text-in/text-out call, and a plain net/http client keeps the
// dependency surface small and the behaviour easy to test with a mocked
// HTTP transport.
//
// The API key is supplied by the caller (read from the GEMINI_API_KEY
// environment variable in main) and sent in the x-goog-api-key header. It is
// never placed in a URL or returned in an error, so it does not leak into
// logs.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultModel is a model listed on Google's API free tier. It is used when
// GEMINI_MODEL is not set. Override it with GEMINI_MODEL to pick another
// free-tier model (e.g. gemini-2.5-flash-lite).
const DefaultModel = "gemini-3.8-flash"

const (
	defaultEndpoint        = "https://generativelanguage.googleapis.com"
	defaultTimeout         = 10 * time.Second
	defaultMaxOutputTokens = 512
	defaultTemperature     = 0.2

	// maxResponseBytes caps the API response body we will read, so a
	// misbehaving upstream cannot exhaust memory.
	maxResponseBytes = 1 << 20

	// Default local rate limit. The free tier allows only a handful of
	// requests per minute, so we throttle conservatively and surface a
	// friendly message rather than burning quota.
	defaultRatePerMinute = 15
	defaultBurst         = 5
)

// Sentinel errors. Callers use errors.Is to map them to user-facing replies.
var (
	// ErrNotConfigured means no API key was provided.
	ErrNotConfigured = errors.New("gemini: API key not configured")
	// ErrRateLimited means the API returned 429/RESOURCE_EXHAUSTED or the
	// local rate limiter rejected the call.
	ErrRateLimited = errors.New("gemini: rate limited")
	// ErrTimeout means the request exceeded its deadline.
	ErrTimeout = errors.New("gemini: request timed out")
	// ErrUnavailable means a network error or a non-429 error status.
	ErrUnavailable = errors.New("gemini: service unavailable")
	// ErrInvalidResponse means the response could not be parsed or carried
	// no usable text (e.g. a blocked or empty candidate).
	ErrInvalidResponse = errors.New("gemini: invalid response")
)

// Client talks to the Gemini generateContent endpoint. The zero value is not
// usable; construct one with New.
type Client struct {
	apiKey          string
	model           string
	endpoint        string
	maxOutputTokens int
	temperature     float64
	timeout         time.Duration
	httpClient      *http.Client
	limiter         *limiter
}

// Option customises a Client.
type Option func(*Client)

// WithEndpoint overrides the API base URL. Used by tests to point at a mock
// server.
func WithEndpoint(url string) Option {
	return func(c *Client) { c.endpoint = strings.TrimRight(url, "/") }
}

// WithHTTPClient sets the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithTimeout sets the per-call timeout applied on top of the caller's
// context. A non-positive value disables the client's own timeout and relies
// on the context alone.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithMaxOutputTokens sets a conservative cap on generated tokens.
func WithMaxOutputTokens(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxOutputTokens = n
		}
	}
}

// WithRateLimit sets the local token-bucket limit: up to perMinute calls with
// the given burst. A non-positive perMinute disables local throttling.
func WithRateLimit(perMinute, burst int) Option {
	return func(c *Client) {
		if perMinute <= 0 {
			c.limiter = nil
			return
		}
		c.limiter = newLimiter(float64(perMinute)/60, float64(burst))
	}
}

// New returns a Client for the given API key and model. An empty model falls
// back to DefaultModel. An empty apiKey is allowed; Answer then returns
// ErrNotConfigured without making a request.
func New(apiKey, model string, opts ...Option) *Client {
	if model == "" {
		model = DefaultModel
	}
	c := &Client{
		apiKey:          apiKey,
		model:           model,
		endpoint:        defaultEndpoint,
		maxOutputTokens: defaultMaxOutputTokens,
		temperature:     defaultTemperature,
		timeout:         defaultTimeout,
		httpClient:      &http.Client{},
		limiter:         newLimiter(float64(defaultRatePerMinute)/60, float64(defaultBurst)),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// wire types for the generateContent request and response.
type genRequest struct {
	Contents          []genContent     `json:"contents"`
	SystemInstruction *genContent      `json:"systemInstruction,omitempty"`
	GenerationConfig  genGenerationCfg `json:"generationConfig"`
}

type genContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []genPart `json:"parts"`
}

type genPart struct {
	Text string `json:"text"`
}

type genGenerationCfg struct {
	MaxOutputTokens int     `json:"maxOutputTokens"`
	Temperature     float64 `json:"temperature"`
}

type genResponse struct {
	Candidates []struct {
		Content      genContent `json:"content"`
		FinishReason string     `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

type genErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Answer sends systemInstruction (guardrails plus read-only task context) and
// the user's question, and returns the model's plain-text reply. The returned
// error, when non-nil, wraps one of the sentinel errors above.
func (c *Client) Answer(ctx context.Context, systemInstruction, question string) (string, error) {
	if c.apiKey == "" {
		return "", ErrNotConfigured
	}
	if c.limiter != nil && !c.limiter.allow(time.Now()) {
		return "", fmt.Errorf("local rate limit exceeded: %w", ErrRateLimited)
	}

	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body := genRequest{
		Contents:         []genContent{{Role: "user", Parts: []genPart{{Text: question}}}},
		GenerationConfig: genGenerationCfg{MaxOutputTokens: c.maxOutputTokens, Temperature: c.temperature},
	}
	if systemInstruction != "" {
		body.SystemInstruction = &genContent{Parts: []genPart{{Text: systemInstruction}}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", ErrInvalidResponse)
	}

	url := c.endpoint + "/v1beta/models/" + c.model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	// The key travels in a header, never the URL, so it stays out of logs.
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return "", fmt.Errorf("request: %w", ErrTimeout)
		}
		return "", fmt.Errorf("request: %w", ErrUnavailable)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("read body: %w", ErrTimeout)
		}
		return "", fmt.Errorf("read body: %w", ErrUnavailable)
	}

	if resp.StatusCode != http.StatusOK {
		return "", c.statusError(resp.StatusCode, data)
	}

	var parsed genResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", ErrInvalidResponse)
	}
	text := extractText(parsed)
	if text == "" {
		return "", fmt.Errorf("no text in response: %w", ErrInvalidResponse)
	}
	return text, nil
}

// statusError classifies a non-200 response. The upstream message is included
// for operator logs; it never contains the API key.
func (c *Client) statusError(code int, data []byte) error {
	var e genErrorBody
	_ = json.Unmarshal(data, &e)
	detail := strings.TrimSpace(e.Error.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(data))
	}
	if code == http.StatusTooManyRequests || e.Error.Status == "RESOURCE_EXHAUSTED" {
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrRateLimited)
	}
	if code >= 500 {
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrUnavailable)
	}
	return fmt.Errorf("status %d: %s: %w", code, detail, ErrInvalidResponse)
}

// extractText joins the text parts of the first candidate.
func extractText(r genResponse) string {
	if len(r.Candidates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// limiter is a tiny token-bucket rate limiter. It refills at ratePerSec and
// holds at most burst tokens.
type limiter struct {
	mu         sync.Mutex
	tokens     float64
	burst      float64
	ratePerSec float64
	last       time.Time
}

func newLimiter(ratePerSec, burst float64) *limiter {
	if burst < 1 {
		burst = 1
	}
	return &limiter{tokens: burst, burst: burst, ratePerSec: ratePerSec}
}

func (l *limiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.last = now
	}
	l.tokens += now.Sub(l.last).Seconds() * l.ratePerSec
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
