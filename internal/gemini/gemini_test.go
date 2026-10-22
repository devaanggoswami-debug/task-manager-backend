package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient returns a client pointed at srv with local rate limiting off
// (so tests exercise the HTTP path rather than the limiter).
func newTestClient(srv *httptest.Server, opts ...Option) *Client {
	base := []Option{WithEndpoint(srv.URL), WithHTTPClient(srv.Client()), WithRateLimit(0, 0)}
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
	for _, want := range []string{`"How is S1?"`, `"maxOutputTokens":512`, `"You are a helpful assistant."`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body missing %q: %s", want, gotBody)
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
