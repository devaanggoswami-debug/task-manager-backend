// Package store persists tasks and their comments in PostgreSQL.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const foreignKeyViolation = "23503"

// ErrNotFound is returned when no task exists with the requested id.
var ErrNotFound = errors.New("task not found")

// Task is a persisted task. DueDate holds a calendar date at midnight UTC.
type Task struct {
	ID          int64
	Assignee    string
	DueDate     time.Time
	Description string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskInput holds the user-editable fields of a task.
type TaskInput struct {
	Assignee    string
	DueDate     time.Time
	Description string
	Status      string
}

// Comment is a persisted status update on a task.
type Comment struct {
	ID        int64
	TaskID    int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// CommentInput holds the user-supplied fields of a comment.
type CommentInput struct {
	Author string
	Body   string
}

// PostgresStore implements task persistence on a pgx connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const (
	taskColumns = `id, assignee, due_date, description, status, created_at, updated_at`

	listSQL   = `SELECT ` + taskColumns + ` FROM tasks ORDER BY due_date ASC, id ASC`
	getSQL    = `SELECT ` + taskColumns + ` FROM tasks WHERE id = $1`
	createSQL = `INSERT INTO tasks (assignee, due_date, description, status) VALUES ($1, $2, $3, $4)
	RETURNING ` + taskColumns
	updateSQL = `UPDATE tasks SET assignee = $1, due_date = $2, description = $3, status = $4, updated_at = now() WHERE id = $5
	RETURNING ` + taskColumns
	deleteSQL     = `DELETE FROM tasks WHERE id = $1`
	taskExistsSQL = `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`

	commentColumns   = `id, task_id, author, body, created_at`
	listCommentsSQL  = `SELECT ` + commentColumns + ` FROM task_comments WHERE task_id = $1 ORDER BY created_at ASC, id ASC`
	createCommentSQL = `INSERT INTO task_comments (task_id, author, body) VALUES ($1, $2, $3)
	RETURNING ` + commentColumns
)

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Assignee, &t.DueDate, &t.Description, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

// List returns all tasks ordered by due date, then id.
func (s *PostgresStore) List(ctx context.Context) ([]Task, error) {
	rows, err := s.pool.Query(ctx, listSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Get returns the task with the given id.
func (s *PostgresStore) Get(ctx context.Context, id int64) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, getSQL, id))
}

// Create inserts a new task.
func (s *PostgresStore) Create(ctx context.Context, in TaskInput) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, createSQL, in.Assignee, in.DueDate, in.Description, in.Status))
}

// Update replaces the editable fields of the task with the given id.
func (s *PostgresStore) Update(ctx context.Context, id int64, in TaskInput) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, updateSQL, in.Assignee, in.DueDate, in.Description, in.Status, id))
}

// Delete removes the task with the given id.
func (s *PostgresStore) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, deleteSQL, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListComments returns the comments of the task with the given id, oldest
// first. It returns ErrNotFound when the task does not exist.
func (s *PostgresStore) ListComments(ctx context.Context, taskID int64) ([]Comment, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, taskExistsSQL, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, listCommentsSQL, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	comments := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// CreateComment adds a comment to the task with the given id. It returns
// ErrNotFound when the task does not exist, including when the task is
// deleted concurrently.
func (s *PostgresStore) CreateComment(ctx context.Context, taskID int64, in CommentInput) (Comment, error) {
	var c Comment
	err := s.pool.QueryRow(ctx, createCommentSQL, taskID, in.Author, in.Body).
		Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &c.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return Comment{}, ErrNotFound
	}
	return c, err
}
