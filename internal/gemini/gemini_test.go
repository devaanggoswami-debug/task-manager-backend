package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a client pointed at srv with local rate limiting off
// (so tests exercise the HTTP path rather than the limiter) and a negligible
// retry backoff (so retry behaviour is exercised without slowing the suite).
// The default retry budget (2 extra attempts) is kept; override it per test
// with WithRetry.
func newTestClient(srv *httptest.Server, opts ...Option) *Client {
	base := []Option{
		WithEndpoint(srv.URL),
		WithHTTPClient(srv.Client()),
		WithRateLimit(0, 0),
		WithRetry(2, time.Millisecond),
	}
	return New("test-key", "gemini-3.8-flash", append(base, opts...)...)
}

func TestAnswerSuccess(t *testing.T) {
	var gotPath, gotKey, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Sprint S1 has 3 open tasks."}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Answer(context.Background(), "You are a helpful assistant.", "How is S1?")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "Sprint S1 has 3 open tasks." {
		t.Fatalf("reply = %q", got)
	}
	if gotPath != "/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	// The key must travel in the header, never the URL.
	if gotKey != "test-key" {
		t.Fatalf("api key header = %q", gotKey)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	for _, want := range []string{
		`"How is S1?"`,
		`"maxOutputTokens":256`,
		`"You are a helpful assistant."`,
		`"thinkingConfig":{"thinkingLevel":"low"}`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body missing %q: %s", want, gotBody)
		}
	}
	// The default model must not be sent the unsupported "minimal" level.
	if strings.Contains(gotBody, `"minimal"`) {
		t.Fatalf("request body must not use the unsupported minimal level: %s", gotBody)
	}
}

// captureServer returns a 200/okBody server that records the last request body
// it received, for asserting on the generationConfig sent.
func captureServer(body *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	}))
}

// TestThinkingLevelAppliedToGemini3 verifies the low thinking level is sent for
// Gemini 3 models that support it, and never the unsupported "minimal".
func TestThinkingLevelAppliedToGemini3(t *testing.T) {
	for _, model := range []string{"gemini-3.8-flash", "gemini-3-pro", "models/gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			var gotBody string
			srv := captureServer(&gotBody)
			defer srv.Close()

			c := New("test-key", model, WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithRateLimit(0, 0))
			if _, err := c.Answer(context.Background(), "sys", "hi"); err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if !strings.Contains(gotBody, `"thinkingConfig":{"thinkingLevel":"low"}`) {
				t.Fatalf("request missing low thinkingLevel: %s", gotBody)
			}
			if strings.Contains(gotBody, `"minimal"`) {
				t.Fatalf("request must not use unsupported minimal level: %s", gotBody)
			}
		})
	}
}

// TestThinkingLevelOmittedForUnsupportedModel verifies that a configured model
// without thinkingLevel support is not sent the parameter, so GEMINI_MODEL
// stays safely configurable.
func TestThinkingLevelOmittedForUnsupportedModel(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash-lite", "gemini-2.5-flash", "gemini-1.5-pro"} {
		t.Run(model, func(t *testing.T) {
			var gotBody string
			srv := captureServer(&gotBody)
			defer srv.Close()

			c := New("test-key", model, WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithRateLimit(0, 0))
			if _, err := c.Answer(context.Background(), "sys", "hi"); err != nil {
				t.Fatalf("Answer: %v", err)
			}
			if strings.Contains(gotBody, "thinkingConfig") || strings.Contains(gotBody, "thinkingLevel") {
				t.Fatalf("unsupported model must not receive thinkingConfig: %s", gotBody)
			}
		})
	}
}

// TestThinkingLevelConfigurable verifies WithThinkingLevel overrides the level,
// and that an empty level omits the config even on a supported model.
func TestThinkingLevelConfigurable(t *testing.T) {
	t.Run("override to high", func(t *testing.T) {
		var gotBody string
		srv := captureServer(&gotBody)
		defer srv.Close()

		c := New("test-key", "gemini-3.8-flash", WithEndpoint(srv.URL), WithHTTPClient(srv.Client()),
			WithRateLimit(0, 0), WithThinkingLevel("high"))
		if _, err := c.Answer(context.Background(), "sys", "hi"); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		if !strings.Contains(gotBody, `"thinkingLevel":"high"`) {
			t.Fatalf("expected high thinkingLevel: %s", gotBody)
		}
	})
	t.Run("empty omits config", func(t *testing.T) {
		var gotBody string
		srv := captureServer(&gotBody)
		defer srv.Close()

		c := New("test-key", "gemini-3.8-flash", WithEndpoint(srv.URL), WithHTTPClient(srv.Client()),
			WithRateLimit(0, 0), WithThinkingLevel(""))
		if _, err := c.Answer(context.Background(), "sys", "hi"); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		if strings.Contains(gotBody, "thinkingConfig") {
			t.Fatalf("empty level must omit thinkingConfig: %s", gotBody)
		}
	})
}

// TestThinkingLevelPresentAcrossQuotaRetries ties the two requirements
// together: a 429 (quota) is retried, and every attempt — including the
// recovering one — still carries the low thinking-level configuration.
func TestThinkingLevelPresentAcrossQuotaRetries(t *testing.T) {
	var calls int32
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("reply = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("got %d request bodies, want 2 (one 429, one success)", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, `"thinkingConfig":{"thinkingLevel":"low"}`) {
			t.Fatalf("attempt %d missing low thinkingLevel: %s", i, b)
		}
	}
}

func TestAnswerMissingAPIKey(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := New("", "gemini-3.8-flash", WithEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.Answer(context.Background(), "sys", "hi")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if called {
		t.Fatal("no HTTP request should be made without an API key")
	}
}

func TestAnswerTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the response until the client has given up
	}))
	defer srv.Close()
	defer close(release)

	c := newTestClient(srv, WithTimeout(50*time.Millisecond))
	start := time.Now()
	_, err := c.Answer(context.Background(), "sys", "hi")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
}

func TestAnswerMalformedResponse(t *testing.T) {
	tests := map[string]string{
		"not json":         `}{not json`,
		"no candidates":    `{"candidates":[]}`,
		"empty parts":      `{"candidates":[{"content":{"parts":[]}}]}`,
		"blocked, no text": `{"candidates":[{"finishReason":"SAFETY"}],"promptFeedback":{"blockReason":"SAFETY"}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer srv.Close()

			_, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestAnswerRateLimitStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestAnswerServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"code":500,"message":"backend error","status":"INTERNAL"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestLocalRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer srv.Close()

	// Burst of 2, slow refill: the third immediate call is throttled locally.
	c := New("k", "m", WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithRateLimit(1, 2))
	for i := 0; i < 2; i++ {
		if _, err := c.Answer(context.Background(), "sys", "hi"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := c.Answer(context.Background(), "sys", "hi"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third call err = %v, want ErrRateLimited", err)
	}
}

// okBody is a minimal valid generateContent success body.
const okBody = `{"candidates":[{"content":{"parts":[{"text":"recovered"}]}}]}`

// flakyServer returns a server whose handler fails for the first failCount
// requests with the given status/body, then returns okBody with 200. It also
// records how many requests it received.
func flakyServer(failCount int, status int, failBody string) (*httptest.Server, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) <= failCount {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, failBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	}))
	return srv, &calls
}

const unavailableBody = `{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}`

// TestAnswerRetriesAfter503 covers the Render failure mode: a 503 "model
// overloaded" response followed by a success. The call should recover on the
// retry and return the reply.
func TestAnswerRetriesAfter503(t *testing.T) {
	srv, calls := flakyServer(1, http.StatusServiceUnavailable, unavailableBody)
	defer srv.Close()

	got, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("reply = %q", got)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("made %d requests, want 2 (one 503, one success)", n)
	}
}

// TestAnswerRepeated503 covers persistent unavailability: every attempt gets a
// 503. The call should exhaust its retries and return ErrUnavailable so the
// handler can show its friendly "temporarily unavailable" reply.
func TestAnswerRepeated503(t *testing.T) {
	srv, calls := flakyServer(100, http.StatusServiceUnavailable, unavailableBody)
	defer srv.Close()

	_, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// 1 initial attempt + 2 retries = 3 requests, never more.
	if n := atomic.LoadInt32(calls); n != 3 {
		t.Fatalf("made %d requests, want 3 (initial + 2 retries)", n)
	}
}

// TestAnswerRetryLimitHonoured checks that WithRetry bounds the number of
// attempts: with 1 extra retry allowed, a persistently failing endpoint is
// hit exactly twice.
func TestAnswerRetryLimitHonoured(t *testing.T) {
	srv, calls := flakyServer(100, http.StatusServiceUnavailable, unavailableBody)
	defer srv.Close()

	c := newTestClient(srv, WithRetry(1, time.Millisecond))
	if _, err := c.Answer(context.Background(), "sys", "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("made %d requests, want 2 (initial + 1 retry)", n)
	}
}

// TestAnswerRetriesOn429 checks that a rate-limit status is treated as
// transient and retried, recovering when the quota frees up.
func TestAnswerRetriesOn429(t *testing.T) {
	srv, calls := flakyServer(2, http.StatusTooManyRequests,
		`{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	defer srv.Close()

	got, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("reply = %q", got)
	}
	if n := atomic.LoadInt32(calls); n != 3 {
		t.Fatalf("made %d requests, want 3", n)
	}
}

// TestAnswerNoRetryOnClientError checks that permanent 4xx failures — a bad
// API key, a permission error, an unknown model, a malformed request — are
// returned immediately without wasting the retry budget.
func TestAnswerNoRetryOnClientError(t *testing.T) {
	cases := map[string]int{
		"bad api key (401)":     http.StatusUnauthorized,
		"permission (403)":      http.StatusForbidden,
		"unknown model (404)":   http.StatusNotFound,
		"malformed request 400": http.StatusBadRequest,
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			srv, calls := flakyServer(100, code,
				`{"error":{"code":`+itoa(code)+`,"message":"nope","status":"X"}}`)
			defer srv.Close()

			_, err := newTestClient(srv).Answer(context.Background(), "sys", "hi")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err = %v, want ErrInvalidResponse", err)
			}
			if n := atomic.LoadInt32(calls); n != 1 {
				t.Fatalf("made %d requests, want exactly 1 (no retry on client error)", n)
			}
		})
	}
}

// TestAnswerRetriesOnTimeout checks that a per-attempt timeout is transient:
// a first attempt that stalls past the timeout is retried, and the second
// (fast) attempt succeeds.
func TestAnswerRetriesOnTimeout(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			time.Sleep(200 * time.Millisecond) // exceed the per-attempt timeout
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	}))
	defer srv.Close()

	c := newTestClient(srv, WithTimeout(40*time.Millisecond))
	got, err := c.Answer(context.Background(), "sys", "hi")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("reply = %q", got)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("made %d requests, want 2 (one timed-out, one success)", n)
	}
}

// TestAnswerStopsRetryOnCallerTimeout checks that retries do not outlive the
// caller's context: once the caller's deadline passes, the call returns
// ErrTimeout promptly rather than running the full retry budget.
func TestAnswerStopsRetryOnCallerTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	// Per-attempt timeout is longer than the caller's deadline, so the
	// caller's context is what trips first; the call must then give up.
	c := newTestClient(srv, WithTimeout(5*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Answer(ctx, "sys", "hi")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call took %v; retries should stop at the caller's deadline", elapsed)
	}
}

// TestAnswerNoRetryAfterContextCancelled checks that a cancelled request
// context stops the call at once: it does not start a fresh retry.
func TestAnswerNoRetryAfterContextCancelled(t *testing.T) {
	srv, calls := flakyServer(100, http.StatusServiceUnavailable, unavailableBody)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call starts

	_, err := newTestClient(srv).Answer(ctx, "sys", "hi")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// At most one attempt may begin; no retry is scheduled after cancellation.
	if n := atomic.LoadInt32(calls); n > 1 {
		t.Fatalf("made %d requests after cancellation, want at most 1", n)
	}
}

// itoa is a tiny int-to-string helper to keep the table above readable.
func itoa(n int) string {
	return strconv.Itoa(n)
}

func TestLimiterRefill(t *testing.T) {
	l := newLimiter(1, 1) // 1 token/sec, burst 1
	now := time.Unix(0, 0)
	if !l.allow(now) {
		t.Fatal("first call should be allowed")
	}
	if l.allow(now) {
		t.Fatal("second immediate call should be denied")
	}
	if !l.allow(now.Add(time.Second)) {
		t.Fatal("call after 1s should be allowed")
	}
}
