package api

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	status := "in_progress"
	valid := taskRequest{Assignee: "  Alice  ", Date: "2026-10-31", Description: " Write docs ", Status: &status}

	tests := []struct {
		name   string
		mutate func(*taskRequest)
		field  string
		msg    string
	}{
		{"missing assignee", func(r *taskRequest) { r.Assignee = "" }, "assignee", "is required"},
		{"blank assignee", func(r *taskRequest) { r.Assignee = " \t\n " }, "assignee", "is required"},
		{"long assignee", func(r *taskRequest) { r.Assignee = strings.Repeat("a", 101) }, "assignee", "must be at most 100 characters"},
		{"NUL in assignee", func(r *taskRequest) { r.Assignee = "a\x00b" }, "assignee", "must not contain NUL characters"},
		{"missing description", func(r *taskRequest) { r.Description = "" }, "description", "is required"},
		{"long description", func(r *taskRequest) { r.Description = strings.Repeat("d", 1001) }, "description", "must be at most 1000 characters"},
		{"missing date", func(r *taskRequest) { r.Date = "" }, "date", "is required"},
		{"impossible date", func(r *taskRequest) { r.Date = "2026-02-30" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"non-leap Feb 29", func(r *taskRequest) { r.Date = "2026-02-29" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"month 13", func(r *taskRequest) { r.Date = "2026-13-01" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"unpadded", func(r *taskRequest) { r.Date = "2026-1-5" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"wrong format", func(r *taskRequest) { r.Date = "10/31/2026" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"with time", func(r *taskRequest) { r.Date = "2026-10-31T00:00:00Z" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"surrounding space", func(r *taskRequest) { r.Date = " 2026-10-31" }, "date", "must be a valid date in YYYY-MM-DD format"},
		{"year zero", func(r *taskRequest) { r.Date = "0000-01-01" }, "date", "must be a valid date in YYYY-MM-DD format"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			_, fields := req.validate(true)
			if got := fields[tc.field]; got != tc.msg {
				t.Fatalf("fields[%q] = %q, want %q (all: %v)", tc.field, got, tc.msg, fields)
			}
			if len(fields) != 1 {
				t.Fatalf("expected exactly one field error, got %v", fields)
			}
		})
	}

	t.Run("valid input is trimmed", func(t *testing.T) {
		in, fields := valid.validate(true)
		if fields != nil {
			t.Fatalf("unexpected errors: %v", fields)
		}
		if in.Assignee != "Alice" || in.Description != "Write docs" || in.Status != "in_progress" {
			t.Fatalf("not trimmed: %+v", in)
		}
		if in.DueDate.Format(dateLayout) != "2026-10-31" {
			t.Fatalf("due date = %v", in.DueDate)
		}
	})

	t.Run("length limits count characters not bytes", func(t *testing.T) {
		req := valid
		req.Assignee = strings.Repeat("é", 100)
		req.Description = strings.Repeat("日", 1000)
		if _, fields := req.validate(true); fields != nil {
			t.Fatalf("unexpected errors: %v", fields)
		}
	})

	t.Run("leap day accepted", func(t *testing.T) {
		req := valid
		req.Date = "2028-02-29"
		if _, fields := req.validate(true); fields != nil {
			t.Fatalf("unexpected errors: %v", fields)
		}
	})

	t.Run("all fields reported together", func(t *testing.T) {
		_, fields := taskRequest{}.validate(true)
		if len(fields) != 4 {
			t.Fatalf("expected 4 field errors, got %v", fields)
		}
	})

	t.Run("status", func(t *testing.T) {
		req := valid
		req.Status = nil
		if in, fields := req.validate(false); fields != nil || in.Status != "not_started" {
			t.Fatalf("omitted optional status: %+v, %v", in, fields)
		}
		if _, fields := req.validate(true); fields["status"] != "is required" || len(fields) != 1 {
			t.Fatalf("omitted required status: %v", fields)
		}
		for _, bad := range []string{"", "DONE", " done", "open", "not started"} {
			req.Status = &bad
			for _, required := range []bool{false, true} {
				if _, fields := req.validate(required); fields["status"] != invalidStatusMsg || len(fields) != 1 {
					t.Fatalf("status %q (required=%v): %v", bad, required, fields)
				}
			}
		}
	})
}

func TestValidateComment(t *testing.T) {
	in, fields := commentRequest{Author: "  Alice ", Body: "\n hello\nworld \t"}.validate()
	if fields != nil || in.Author != "Alice" || in.Body != "hello\nworld" {
		t.Fatalf("valid comment: %+v, %v", in, fields)
	}
	if _, fields := (commentRequest{Author: strings.Repeat("é", 100), Body: strings.Repeat("日", 2000)}).validate(); fields != nil {
		t.Fatalf("multibyte at limit: %v", fields)
	}
	if _, fields := (commentRequest{Author: strings.Repeat("é", 101), Body: strings.Repeat("日", 2001)}).validate(); fields["author"] == "" || fields["body"] == "" {
		t.Fatalf("multibyte over limit: %v", fields)
	}
}
