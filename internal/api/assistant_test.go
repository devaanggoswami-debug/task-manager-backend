package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskmanager/backend/internal/gemini"
)

// stubAssistant records the last call and returns a canned reply or error.
type stubAssistant struct {
	reply       string
	err         error
	calls       int
	gotQuestion string
	gotContext  string
}

func (s *stubAssistant) Answer(_ context.Context, systemInstruction, question string) (string, error) {
	s.calls++
	s.gotContext = systemInstruction
	s.gotQuestion = question
	return s.reply, s.err
}

func newAssistantHandler(s TaskStore, a Assistant) http.Handler {
	return newHandler(s, testOrigin, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return fixedNow }, a)
}

func TestChatAssistantSuccess(t *testing.T) {
	fs := newFakeStore()
	stub := &stubAssistant{reply: "  Sprint S1 has 2 tasks and looks on track.  "}
	h := newAssistantHandler(fs, stub)
	seed(t, h, fs)
	chat(t, h, `create task "Fix login" for Alice due 2026-10-27 in sprint S1 project Auth`)

	got := chat(t, h, "which tasks are assigned to Alice?")
	if got.Reply != "Sprint S1 has 2 tasks and looks on track." { // trimmed
		t.Fatalf("reply = %q", got.Reply)
	}
	if got.Changed || got.Task != nil || got.Status != nil {
		t.Fatalf("got %+v", got)
	}
	if stub.calls != 1 {
		t.Fatalf("assistant calls = %d", stub.calls)
	}
	if stub.gotQuestion != "which tasks are assigned to Alice?" {
		t.Fatalf("question = %q", stub.gotQuestion)
	}
	// The context must carry the guardrails and the user's data.
	for _, want := range []string{"Today is 2026-10-21", "ONLY the data", "cannot create", "Projects (2)", "Sprints (2)", "Fix login", "Alice"} {
		if !strings.Contains(stub.gotContext, want) {
			t.Fatalf("context missing %q:\n%s", want, stub.gotContext)
		}
	}
}

func TestChatAssistantErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		contains string
	}{
		{"rate limit", fmt.Errorf("x: %w", gemini.ErrRateLimited), "request limit"},
		{"timeout", fmt.Errorf("x: %w", gemini.ErrTimeout), "took too long"},
		{"unavailable", fmt.Errorf("x: %w", gemini.ErrUnavailable), "temporarily unavailable"},
		{"invalid", fmt.Errorf("x: %w", gemini.ErrInvalidResponse), "temporarily unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			stub := &stubAssistant{err: tc.err}
			h := newAssistantHandler(fs, stub)
			got := chat(t, h, "how are things going?")
			if got.Changed || got.Task != nil || got.Status != nil {
				t.Fatalf("got %+v", got)
			}
			if !strings.Contains(got.Reply, tc.contains) {
				t.Fatalf("reply = %q, want to contain %q", got.Reply, tc.contains)
			}
		})
	}
}

func TestChatAssistantEmptyReply(t *testing.T) {
	fs := newFakeStore()
	stub := &stubAssistant{reply: "   "}
	h := newAssistantHandler(fs, stub)
	got := chat(t, h, "anything?")
	if !strings.Contains(got.Reply, "temporarily unavailable") {
		t.Fatalf("reply = %q", got.Reply)
	}
}

// TestChatAssistantNotCalledForCommands checks that the deterministic create
// and status intents never reach the model (so they consume no quota).
func TestChatAssistantNotCalledForCommands(t *testing.T) {
	fs := newFakeStore()
	stub := &stubAssistant{reply: "should not be used"}
	h := newAssistantHandler(fs, stub)
	seed(t, h, fs)

	chat(t, h, `create task "Fix login" for Alice due 2026-10-27 in sprint S1 project Auth`)
	chat(t, h, "what's the status of sprint S1?")
	if stub.calls != 0 {
		t.Fatalf("assistant was called %d times for command intents", stub.calls)
	}
}

// TestChatNoAssistantFallsBackToHelp checks that without an assistant a
// free-form message still returns the static help reply.
func TestChatNoAssistantFallsBackToHelp(t *testing.T) {
	h := newAssistantHandler(newFakeStore(), nil)
	got := chat(t, h, "hello there")
	if got.Reply != helpReply {
		t.Fatalf("reply = %q", got.Reply)
	}
}
