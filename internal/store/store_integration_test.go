package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresStoreIntegration runs against a real database that already has
// db/schema.sql applied. Set TEST_DATABASE_URL to enable it.
func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	s := NewPostgresStore(pool)
	var created []int64
	t.Cleanup(func() {
		for _, id := range created {
			if _, err := pool.Exec(context.Background(), "DELETE FROM tasks WHERE id = $1", id); err != nil {
				t.Errorf("cleanup task %d: %v", id, err)
			}
		}
	})

	later := time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)
	earlier := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	a, err := s.Create(ctx, TaskInput{Assignee: "Alice", DueDate: later, Description: "'; DROP TABLE tasks; --", Status: "not_started"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	created = append(created, a.ID)
	b, err := s.Create(ctx, TaskInput{Assignee: "Bob", DueDate: earlier, Description: "earlier", Status: "blocked"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	created = append(created, b.ID)

	if a.ID <= 0 || !a.DueDate.Equal(later) || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.Status != "not_started" || b.Status != "blocked" {
		t.Fatalf("unexpected created tasks: %+v %+v", a, b)
	}

	got, err := s.Get(ctx, a.ID)
	if err != nil || got.Description != "'; DROP TABLE tasks; --" || got.DueDate.Format("2006-01-02") != "2099-12-31" || got.Status != "not_started" {
		t.Fatalf("get: %+v, %v", got, err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	posA, posB := -1, -1
	for i, task := range list {
		switch task.ID {
		case a.ID:
			posA = i
		case b.ID:
			posB = i
			if task.Status != "blocked" {
				t.Fatalf("list status: %+v", task)
			}
		}
	}
	if posA < 0 || posB < 0 || posB > posA {
		t.Fatalf("list ordering wrong: posA=%d posB=%d", posA, posB)
	}

	upd, err := s.Update(ctx, a.ID, TaskInput{Assignee: "Carol", DueDate: earlier, Description: "changed", Status: "in_progress"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Assignee != "Carol" || !upd.DueDate.Equal(earlier) || upd.UpdatedAt.Before(a.UpdatedAt) || !upd.CreatedAt.Equal(a.CreatedAt) || upd.Status != "in_progress" {
		t.Fatalf("unexpected updated task: %+v", upd)
	}
	if got, err := s.Get(ctx, a.ID); err != nil || got.Status != "in_progress" {
		t.Fatalf("get after update: %+v, %v", got, err)
	}
	if _, err := s.Update(ctx, a.ID, TaskInput{Assignee: "Carol", DueDate: earlier, Description: "changed", Status: "bogus"}); err == nil {
		t.Fatalf("status check constraint not enforced")
	}

	comments, err := s.ListComments(ctx, a.ID)
	if err != nil || comments == nil || len(comments) != 0 {
		t.Fatalf("list comments on new task: %#v, %v", comments, err)
	}
	c1, err := s.CreateComment(ctx, a.ID, CommentInput{Author: "Alice", Body: "first\nupdate"})
	if err != nil {
		t.Fatalf("create comment 1: %v", err)
	}
	c2, err := s.CreateComment(ctx, a.ID, CommentInput{Author: "Bob", Body: "'; DELETE FROM task_comments; --"})
	if err != nil {
		t.Fatalf("create comment 2: %v", err)
	}
	if c1.ID <= 0 || c1.TaskID != a.ID || c1.Author != "Alice" || c1.Body != "first\nupdate" || c1.CreatedAt.IsZero() || c2.ID <= c1.ID {
		t.Fatalf("unexpected comments: %+v %+v", c1, c2)
	}
	if _, err := s.CreateComment(ctx, b.ID, CommentInput{Author: "Eve", Body: "other task"}); err != nil {
		t.Fatalf("create comment on b: %v", err)
	}
	comments, err = s.ListComments(ctx, a.ID)
	if err != nil || len(comments) != 2 || comments[0].ID != c1.ID || comments[1].ID != c2.ID || comments[1].Body != c2.Body {
		t.Fatalf("list comments order: %+v, %v", comments, err)
	}
	if after, err := s.Get(ctx, a.ID); err != nil || !after.UpdatedAt.Equal(upd.UpdatedAt) {
		t.Fatalf("comment changed task updated_at: %+v, %v", after, err)
	}

	if err := s.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM task_comments WHERE task_id = $1", a.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("comments not cascaded: count=%d, %v", remaining, err)
	}
	if bc, err := s.ListComments(ctx, b.ID); err != nil || len(bc) != 1 {
		t.Fatalf("cascade touched other task: %+v, %v", bc, err)
	}
	if _, err := s.Get(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.Delete(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.Update(ctx, a.ID, TaskInput{Assignee: "x", DueDate: earlier, Description: "x", Status: "done"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := s.ListComments(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list comments on missing task: %v", err)
	}
	if _, err := s.CreateComment(ctx, a.ID, CommentInput{Author: "x", Body: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create comment on missing task: %v", err)
	}
}
