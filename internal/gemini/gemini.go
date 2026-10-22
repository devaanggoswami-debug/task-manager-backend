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
	"math/rand/v2"
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
	defaultEndpoint = "https://generativelanguage.googleapis.com"
	// defaultTimeout bounds a single HTTP attempt (not the whole call). The
	// free-tier endpoint can be slow under load, so 10s was too tight. 15s
	// gives a stalled-but-recoverable request room to finish while leaving the
	// retry budget below time to run within the caller's deadline (the
	// assistant handler allows 55s). The total call time stays bounded by the
	// retry budget and the caller's context.
	defaultTimeout         = 15 * time.Second
	defaultMaxOutputTokens = 512
	defaultTemperature     = 0.2

	// Retry defaults. On a transient upstream failure (503/429/408, other
	// 5xx, or a network/timeout error) the call is retried up to
	// defaultMaxRetries extra times, waiting an exponentially growing,
	// jittered delay (starting at defaultRetryBaseDelay, capped at
	// defaultRetryMaxDelay) between attempts. Retries are few and the waits
	// are capped so the total time stays bounded.
	defaultMaxRetries     = 2
	defaultRetryBaseDelay = 500 * time.Millisecond
	defaultRetryMaxDelay  = 4 * time.Second

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
	maxRetries      int
	retryBaseDelay  time.Duration
	retryMaxDelay   time.Duration
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

// WithRetry sets the retry budget and base backoff for transient failures.
// maxRetries is the number of additional attempts after the first (clamped to
// >= 0; 0 disables retries). baseDelay is the initial backoff, doubled each
// retry with jitter and capped internally; a non-positive baseDelay retries
// without waiting (useful in tests). The per-attempt timeout is unchanged and
// still set with WithTimeout.
func WithRetry(maxRetries int, baseDelay time.Duration) Option {
	return func(c *Client) {
		if maxRetries < 0 {
			maxRetries = 0
		}
		c.maxRetries = maxRetries
		c.retryBaseDelay = baseDelay
	}
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
		maxRetries:      defaultMaxRetries,
		retryBaseDelay:  defaultRetryBaseDelay,
		retryMaxDelay:   defaultRetryMaxDelay,
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
	// The local rate limiter is checked once per logical call, not per
	// attempt: retries are a bounded recovery from a transient upstream
	// failure, not new user requests, so they do not re-consume quota.
	if c.limiter != nil && !c.limiter.allow(time.Now()) {
		return "", fmt.Errorf("local rate limit exceeded: %w", ErrRateLimited)
	}

	payload, err := c.marshalRequest(systemInstruction, question)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", ErrInvalidResponse)
	}
	url := c.endpoint + "/v1beta/models/" + c.model + ":generateContent"

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Back off before retrying, aborting early if the caller's
			// context is cancelled or its deadline passes.
			if err := c.waitBackoff(ctx, attempt-1); err != nil {
				return "", lastErr
			}
		}

		text, err, retryable := c.doAttempt(ctx, url, payload)
		if err == nil {
			return text, nil
		}
		lastErr = err
		// Stop immediately on a permanent error (bad key, permission,
		// unknown model, malformed request, unparseable body) or once the
		// caller has given up. Only transient failures reach another
		// iteration, and never more than c.maxRetries times.
		if !retryable || ctx.Err() != nil {
			return "", err
		}
	}
	return "", lastErr
}

// marshalRequest builds the generateContent request body.
func (c *Client) marshalRequest(systemInstruction, question string) ([]byte, error) {
	body := genRequest{
		Contents:         []genContent{{Role: "user", Parts: []genPart{{Text: question}}}},
		GenerationConfig: genGenerationCfg{MaxOutputTokens: c.maxOutputTokens, Temperature: c.temperature},
	}
	if systemInstruction != "" {
		body.SystemInstruction = &genContent{Parts: []genPart{{Text: systemInstruction}}}
	}
	return json.Marshal(body)
}

// doAttempt performs one HTTP attempt. It returns the reply text on success,
// or an error wrapping a sentinel plus whether that error is transient and so
// worth retrying.
func (c *Client) doAttempt(ctx context.Context, url string, payload []byte) (string, error, bool) {
	// Each attempt gets its own timeout derived from the caller's context, so
	// a single stalled attempt cannot consume the whole retry budget.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", ErrUnavailable), false
	}
	req.Header.Set("Content-Type", "application/json")
	// The key travels in a header, never the URL, so it stays out of logs.
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			// A timeout is transient; the caller-level check in Answer stops
			// retries once the caller's own deadline is the one that tripped.
			return "", fmt.Errorf("request: %w", ErrTimeout), true
		}
		return "", fmt.Errorf("request: %w", ErrUnavailable), true
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("read body: %w", ErrTimeout), true
		}
		return "", fmt.Errorf("read body: %w", ErrUnavailable), true
	}

	if resp.StatusCode != http.StatusOK {
		err, retryable := c.statusError(resp.StatusCode, data)
		return "", err, retryable
	}

	var parsed genResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", ErrInvalidResponse), false
	}
	text := extractText(parsed)
	if text == "" {
		return "", fmt.Errorf("no text in response: %w", ErrInvalidResponse), false
	}
	return text, nil, false
}

// statusError classifies a non-200 response into a sentinel error and reports
// whether it is transient (worth retrying). The upstream message is included
// for operator logs; it never contains the API key.
//
// Retryable: 429/RESOURCE_EXHAUSTED (rate limited), 408 (request timeout), and
// any 5xx (the 503 "model overloaded" case included). Not retryable: 400
// (malformed request), 401/403 (bad key or permission), 404 (unknown model),
// and any other 4xx — retrying those just wastes the budget on a deterministic
// failure.
func (c *Client) statusError(code int, data []byte) (error, bool) {
	var e genErrorBody
	_ = json.Unmarshal(data, &e)
	detail := strings.TrimSpace(e.Error.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(data))
	}
	switch {
	case code == http.StatusTooManyRequests || e.Error.Status == "RESOURCE_EXHAUSTED":
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrRateLimited), true
	case code == http.StatusRequestTimeout: // 408
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrTimeout), true
	case code >= 500:
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrUnavailable), true
	default:
		return fmt.Errorf("status %d: %s: %w", code, detail, ErrInvalidResponse), false
	}
}

// waitBackoff sleeps before retry number retryIndex (0 for the first retry),
// returning the context error if the caller's context ends first.
func (c *Client) waitBackoff(ctx context.Context, retryIndex int) error {
	d := c.backoffDelay(retryIndex)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffDelay computes the delay before retry retryIndex: an exponential
// base*2^retryIndex, capped at retryMaxDelay, with equal jitter (a random
// point in the upper half of that interval) to spread out concurrent retries.
func (c *Client) backoffDelay(retryIndex int) time.Duration {
	base := c.retryBaseDelay
	if base <= 0 {
		return 0
	}
	max := c.retryMaxDelay
	if max <= 0 || max < base {
		max = base
	}
	d := base
	for i := 0; i < retryIndex; i++ {
		if d >= max/2 { // doubling would reach or exceed the cap
			d = max
			break
		}
		d <<= 1
	}
	if d > max {
		d = max
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
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
