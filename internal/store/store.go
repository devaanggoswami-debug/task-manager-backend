// Package store persists tasks, their comments, sprints and projects in
// PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
)

// ErrNotFound is returned when no row exists with the requested id.
var ErrNotFound = errors.New("task not found")

// ErrConflict is returned when a sprint or project name is already taken.
var ErrConflict = errors.New("name already exists")

// ErrUnknownProject is returned when a sprint names a project that does not
// exist.
var ErrUnknownProject = errors.New("unknown project")

// ErrSprintNotFound and ErrNotInSprint are returned by RemoveTaskFromSprint
// for a missing sprint and a task that is not in the sprint.
var (
	ErrSprintNotFound = errors.New("sprint not found")
	ErrNotInSprint    = errors.New("task is not in that sprint")
)

// Unclassified is the TaskFilter.Category value that selects tasks without
// a category.
const Unclassified = "unclassified"

// UnknownIDsError reports sprint, project or task ids that do not exist.
// The write that produced it has been rolled back.
type UnknownIDsError struct {
	SprintIDs  []int64
	ProjectIDs []int64
	TaskIDs    []int64
}

func (e *UnknownIDsError) Error() string {
	var parts []string
	if len(e.SprintIDs) > 0 {
		parts = append(parts, "unknown sprint id(s): "+JoinIDs(e.SprintIDs))
	}
	if len(e.ProjectIDs) > 0 {
		parts = append(parts, "unknown project id(s): "+JoinIDs(e.ProjectIDs))
	}
	if len(e.TaskIDs) > 0 {
		parts = append(parts, "unknown task id(s): "+JoinIDs(e.TaskIDs))
	}
	return strings.Join(parts, "; ")
}

// JoinIDs formats ids as "7, 9".
func JoinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ", ")
}

// Task is a persisted task. DueDate holds a calendar date at midnight UTC.
// SprintIDs and ProjectIDs are ascending and never nil.
type Task struct {
	ID          int64
	Assignee    string
	DueDate     time.Time
	Description string
	Status      string
	Category    *string
	SprintIDs   []int64
	ProjectIDs  []int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskInput holds the user-editable fields of a task. On Update a nil
// Category keeps the stored one, and a nil SprintIDs or ProjectIDs leaves
// those memberships unchanged; a non-nil slice replaces them. On Create nil
// means none.
type TaskInput struct {
	Assignee    string
	DueDate     time.Time
	Description string
	Status      string
	Category    *string
	SprintIDs   []int64
	ProjectIDs  []int64
}

// Page selects a window of a list. A zero Limit means the whole list.
type Page struct {
	Limit  int64
	Offset int64
}

func (p Page) limitArg() *int64 {
	if p.Limit == 0 {
		return nil
	}
	return &p.Limit
}

// TaskFilter narrows task lists; all conditions combine with AND. A zero id
// or empty string means no filter. Category may be Unclassified. Q is a
// case-insensitive literal substring of the assignee or description.
// ExcludeSprintID keeps only tasks not in that sprint.
type TaskFilter struct {
	SprintID        int64
	ProjectID       int64
	Status          string
	Category        string
	Q               string
	ExcludeSprintID int64
	Page            Page
}

// SprintFilter narrows sprint lists. A zero ProjectID means any project;
// Unassigned keeps only sprints without a project.
type SprintFilter struct {
	ProjectID  int64
	Unassigned bool
	Page       Page
}

// ProjectFilter narrows project lists. Q is a case-insensitive literal
// substring of the name; empty means no filter.
type ProjectFilter struct {
	Q    string
	Page Page
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

// Sprint is a persisted sprint. Dates are calendar dates at midnight UTC.
// ProjectID is nil for a sprint that belongs to no project.
type Sprint struct {
	ID        int64
	Name      string
	StartDate time.Time
	EndDate   time.Time
	ProjectID *int64
	TaskCount int
	CreatedAt time.Time
}

// SprintInput holds the user-supplied fields of a sprint. A nil ProjectID
// stores no project.
type SprintInput struct {
	Name      string
	StartDate time.Time
	EndDate   time.Time
	ProjectID *int64
}

// Project is a persisted project.
type Project struct {
	ID          int64
	Name        string
	TaskCount   int
	SprintCount int
	CreatedAt   time.Time
}

// ProjectInput holds the user-supplied fields of a project.
type ProjectInput struct {
	Name string
}

// PostgresStore implements persistence on a pgx connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const (
	taskColumns = `t.id, t.assignee, t.due_date, t.description, t.status, t.category, t.created_at, t.updated_at,
	COALESCE((SELECT array_agg(st.sprint_id ORDER BY st.sprint_id) FROM sprint_tasks st WHERE st.task_id = t.id), '{}'::bigint[]),
	COALESCE((SELECT array_agg(pt.project_id ORDER BY pt.project_id) FROM project_tasks pt WHERE pt.task_id = t.id), '{}'::bigint[])`

	taskWhere = ` FROM tasks t
	WHERE ($1::bigint = 0 OR EXISTS (SELECT 1 FROM sprint_tasks st WHERE st.task_id = t.id AND st.sprint_id = $1::bigint))
	  AND ($2::bigint = 0 OR EXISTS (SELECT 1 FROM project_tasks pt WHERE pt.task_id = t.id AND pt.project_id = $2::bigint))
	  AND ($3::text = '' OR t.status = $3::text)
	  AND ($4::text = '' OR ($4::text = 'unclassified' AND t.category IS NULL) OR t.category = $4::text)
	  AND ($5::text = '' OR t.assignee ILIKE '%' || $5::text || '%' ESCAPE '\' OR t.description ILIKE '%' || $5::text || '%' ESCAPE '\')
	  AND ($6::bigint = 0 OR NOT EXISTS (SELECT 1 FROM sprint_tasks st WHERE st.task_id = t.id AND st.sprint_id = $6::bigint))`
	countTasksSQL = `SELECT count(*)` + taskWhere
	listSQL       = `SELECT ` + taskColumns + taskWhere + `
	ORDER BY t.due_date ASC, t.id ASC LIMIT $7::bigint OFFSET $8::bigint`
	getSQL    = `SELECT ` + taskColumns + ` FROM tasks t WHERE t.id = $1`
	createSQL = `INSERT INTO tasks (assignee, due_date, description, status, category) VALUES ($1, $2, $3, $4, $5)
	RETURNING id`
	updateSQL = `UPDATE tasks SET assignee = $1, due_date = $2, description = $3, status = $4,
	category = COALESCE($5::varchar, category), updated_at = now() WHERE id = $6
	RETURNING id`
	deleteSQL     = `DELETE FROM tasks WHERE id = $1`
	taskExistsSQL = `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`

	unknownSprintIDsSQL = `SELECT COALESCE(array_agg(u.id ORDER BY u.id), '{}'::bigint[])
	FROM (SELECT DISTINCT unnest($1::bigint[]) AS id) u WHERE NOT EXISTS (SELECT 1 FROM sprints s WHERE s.id = u.id)`
	unknownProjectIDsSQL = `SELECT COALESCE(array_agg(u.id ORDER BY u.id), '{}'::bigint[])
	FROM (SELECT DISTINCT unnest($1::bigint[]) AS id) u WHERE NOT EXISTS (SELECT 1 FROM projects p WHERE p.id = u.id)`
	clearSprintsSQL   = `DELETE FROM sprint_tasks WHERE task_id = $1`
	clearProjectsSQL  = `DELETE FROM project_tasks WHERE task_id = $1`
	insertSprintsSQL  = `INSERT INTO sprint_tasks (sprint_id, task_id) SELECT DISTINCT unnest($1::bigint[]), $2::bigint`
	insertProjectsSQL = `INSERT INTO project_tasks (project_id, task_id) SELECT DISTINCT unnest($1::bigint[]), $2::bigint`
	// insertSprintProjectsSQL adds the projects of the task's sprints to it.
	insertSprintProjectsSQL = `INSERT INTO project_tasks (project_id, task_id)
	SELECT DISTINCT s.project_id, st.task_id FROM sprint_tasks st JOIN sprints s ON s.id = st.sprint_id
	WHERE st.task_id = $1 AND s.project_id IS NOT NULL
	ON CONFLICT DO NOTHING`

	commentColumns   = `id, task_id, author, body, created_at`
	countCommentsSQL = `SELECT count(*) FROM task_comments WHERE task_id = $1`
	listCommentsSQL  = `SELECT ` + commentColumns + ` FROM task_comments WHERE task_id = $1
	ORDER BY created_at ASC, id ASC LIMIT $2::bigint OFFSET $3::bigint`
	createCommentSQL = `INSERT INTO task_comments (task_id, author, body) VALUES ($1, $2, $3)
	RETURNING ` + commentColumns

	sprintColumns = `s.id, s.name, s.start_date, s.end_date, s.project_id,
	(SELECT count(*) FROM sprint_tasks st WHERE st.sprint_id = s.id), s.created_at`
	sprintWhere = ` FROM sprints s
	WHERE ($1::bigint = 0 OR s.project_id = $1::bigint) AND (NOT $2::boolean OR s.project_id IS NULL)`
	countSprintsSQL = `SELECT count(*)` + sprintWhere
	listSprintsSQL  = `SELECT ` + sprintColumns + sprintWhere + `
	ORDER BY s.start_date ASC, s.id ASC LIMIT $3::bigint OFFSET $4::bigint`
	getSprintSQL    = `SELECT ` + sprintColumns + ` FROM sprints s WHERE s.id = $1`
	createSprintSQL = `INSERT INTO sprints (name, start_date, end_date, project_id) VALUES ($1, $2, $3, $4)
	RETURNING id, name, start_date, end_date, project_id, 0::bigint, created_at`
	lockSprintSQL   = `SELECT project_id FROM sprints WHERE id = $1 FOR UPDATE`
	shareSprintSQL  = `SELECT project_id FROM sprints WHERE id = $1 FOR SHARE`
	updateSprintSQL = `UPDATE sprints SET name = $1, start_date = $2, end_date = $3, project_id = $4 WHERE id = $5`
	// addProjectToSprintTasksSQL adds a project to every task in a sprint.
	addProjectToSprintTasksSQL = `INSERT INTO project_tasks (project_id, task_id)
	SELECT $1::bigint, st.task_id FROM sprint_tasks st WHERE st.sprint_id = $2::bigint
	ON CONFLICT DO NOTHING`
	shareTasksSQL     = `SELECT id FROM tasks WHERE id = ANY($1::bigint[]) FOR SHARE`
	addSprintTasksSQL = `INSERT INTO sprint_tasks (sprint_id, task_id) SELECT $1::bigint, u.id FROM unnest($2::bigint[]) AS u(id)
	ON CONFLICT DO NOTHING`
	addProjectTasksSQL = `INSERT INTO project_tasks (project_id, task_id) SELECT $1::bigint, u.id FROM unnest($2::bigint[]) AS u(id)
	ON CONFLICT DO NOTHING`
	removeSprintTaskSQL = `WITH d AS (DELETE FROM sprint_tasks WHERE sprint_id = $1 AND task_id = $2 RETURNING 1)
	SELECT EXISTS (SELECT 1 FROM sprints WHERE id = $1), EXISTS (SELECT 1 FROM tasks WHERE id = $2), EXISTS (SELECT 1 FROM d)`

	projectColumns = `p.id, p.name,
	(SELECT count(*) FROM project_tasks pt WHERE pt.project_id = p.id),
	(SELECT count(*) FROM sprints s WHERE s.project_id = p.id), p.created_at`
	projectWhere     = ` FROM projects p WHERE ($1::text = '' OR p.name ILIKE '%' || $1::text || '%' ESCAPE '\')`
	countProjectsSQL = `SELECT count(*)` + projectWhere
	listProjectsSQL  = `SELECT ` + projectColumns + projectWhere + `
	ORDER BY lower(p.name) ASC, p.id ASC LIMIT $2::bigint OFFSET $3::bigint`
	getProjectSQL    = `SELECT ` + projectColumns + ` FROM projects p WHERE p.id = $1`
	createProjectSQL = `INSERT INTO projects (name) VALUES ($1)
	RETURNING id, name, 0::bigint, 0::bigint, created_at`
	updateProjectSQL = `UPDATE projects p SET name = $1 WHERE p.id = $2
	RETURNING ` + projectColumns
)

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike makes s match literally inside an ILIKE pattern that uses
// ESCAPE '\'.
func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

// readSnapshot runs fn in a read-only repeatable-read transaction, so a
// page and its total come from one snapshot.
func readSnapshot(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// collectPage runs listSQL with args plus the page's limit and offset, and
// countSQL with args for the total. An unpaginated list is its own total.
func collectPage[T any](ctx context.Context, tx pgx.Tx, countSQL, listSQL string, args []any, p Page,
	scan func(pgx.Row) (T, error)) ([]T, int, error) {
	rows, err := tx.Query(ctx, listSQL, append(slices.Clip(args), p.limitArg(), p.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
	if err != nil {
		return nil, 0, err
	}
	if p.Limit == 0 && p.Offset == 0 {
		return items, len(items), nil
	}
	var total int
	if err := tx.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Assignee, &t.DueDate, &t.Description, &t.Status, &t.Category, &t.CreatedAt, &t.UpdatedAt,
		&t.SprintIDs, &t.ProjectIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if t.SprintIDs == nil {
		t.SprintIDs = []int64{}
	}
	if t.ProjectIDs == nil {
		t.ProjectIDs = []int64{}
	}
	return t, err
}

// List returns all tasks ordered by due date, then id.
func (s *PostgresStore) List(ctx context.Context) ([]Task, error) {
	return s.ListFiltered(ctx, TaskFilter{})
}

// ListFiltered returns the tasks matching f, ordered by due date, then id.
// An unknown sprint or project id yields an empty list.
func (s *PostgresStore) ListFiltered(ctx context.Context, f TaskFilter) ([]Task, error) {
	tasks, _, err := s.ListTasksPage(ctx, f)
	return tasks, err
}

// ListTasksPage returns f's page of the matching tasks, ordered by due
// date, then id, and the number of tasks matching f.
func (s *PostgresStore) ListTasksPage(ctx context.Context, f TaskFilter) ([]Task, int, error) {
	args := []any{f.SprintID, f.ProjectID, f.Status, f.Category, escapeLike(f.Q), f.ExcludeSprintID}
	var tasks []Task
	var total int
	err := readSnapshot(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		tasks, total, err = collectPage(ctx, tx, countTasksSQL, listSQL, args, f.Page, scanTask)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return tasks, total, nil
}

// Get returns the task with the given id.
func (s *PostgresStore) Get(ctx context.Context, id int64) (Task, error) {
	return scanTask(s.pool.QueryRow(ctx, getSQL, id))
}

// Create inserts a new task and its memberships in one transaction. It
// returns *UnknownIDsError when a sprint or project id does not exist.
func (s *PostgresStore) Create(ctx context.Context, in TaskInput) (Task, error) {
	var t Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, createSQL, in.Assignee, in.DueDate, in.Description, in.Status, in.Category).Scan(&id); err != nil {
			return err
		}
		if err := replaceMemberships(ctx, tx, id, nonNil(in.SprintIDs), nonNil(in.ProjectIDs)); err != nil {
			return err
		}
		var err error
		t, err = scanTask(tx.QueryRow(ctx, getSQL, id))
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

// Update replaces the editable fields of the task with the given id, and
// the memberships whose id slice is non-nil, in one transaction. It returns
// ErrNotFound for a missing task and *UnknownIDsError for unknown ids.
func (s *PostgresStore) Update(ctx context.Context, id int64, in TaskInput) (Task, error) {
	var t Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, updateSQL, in.Assignee, in.DueDate, in.Description, in.Status, in.Category, id).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := replaceMemberships(ctx, tx, id, in.SprintIDs, in.ProjectIDs); err != nil {
			return err
		}
		t, err = scanTask(tx.QueryRow(ctx, getSQL, id))
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// replaceMemberships replaces the task's sprint and project sets for each
// non-nil slice, after checking that every id exists. Either replacement
// then adds the projects of the task's sprints, so a task in a project's
// sprint stays in that project.
func replaceMemberships(ctx context.Context, tx pgx.Tx, taskID int64, sprintIDs, projectIDs []int64) error {
	var unknown UnknownIDsError
	if sprintIDs != nil {
		if err := tx.QueryRow(ctx, unknownSprintIDsSQL, sprintIDs).Scan(&unknown.SprintIDs); err != nil {
			return err
		}
	}
	if projectIDs != nil {
		if err := tx.QueryRow(ctx, unknownProjectIDsSQL, projectIDs).Scan(&unknown.ProjectIDs); err != nil {
			return err
		}
	}
	if len(unknown.SprintIDs) > 0 || len(unknown.ProjectIDs) > 0 {
		return &unknown
	}

	for _, m := range []struct {
		ids           []int64
		clear, insert string
	}{
		{sprintIDs, clearSprintsSQL, insertSprintsSQL},
		{projectIDs, clearProjectsSQL, insertProjectsSQL},
	} {
		if m.ids == nil {
			continue
		}
		if _, err := tx.Exec(ctx, m.clear, taskID); err != nil {
			return err
		}
		if len(m.ids) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, m.insert, m.ids, taskID); err != nil {
			return fmt.Errorf("insert memberships: %w", err)
		}
	}
	if sprintIDs != nil || projectIDs != nil {
		if _, err := tx.Exec(ctx, insertSprintProjectsSQL, taskID); err != nil {
			return fmt.Errorf("insert sprint projects: %w", err)
		}
	}
	return nil
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
	comments, _, err := s.ListCommentsPage(ctx, taskID, Page{})
	return comments, err
}

// ListCommentsPage returns page p of the task's comments, oldest first, and
// the task's comment count. It returns ErrNotFound when the task does not
// exist.
func (s *PostgresStore) ListCommentsPage(ctx context.Context, taskID int64, p Page) ([]Comment, int, error) {
	var comments []Comment
	var total int
	err := readSnapshot(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, taskExistsSQL, taskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		var err error
		comments, total, err = collectPage(ctx, tx, countCommentsSQL, listCommentsSQL, []any{taskID}, p, scanComment)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return comments, total, nil
}

func scanComment(row pgx.Row) (Comment, error) {
	var c Comment
	err := row.Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &c.CreatedAt)
	return c, err
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

func scanSprint(row pgx.Row) (Sprint, error) {
	var sp Sprint
	err := row.Scan(&sp.ID, &sp.Name, &sp.StartDate, &sp.EndDate, &sp.ProjectID, &sp.TaskCount, &sp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sprint{}, ErrNotFound
	}
	return sp, err
}

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.TaskCount, &p.SprintCount, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}

// sprintWriteError maps constraint violations of a sprint write.
func sprintWriteError(err error) error {
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case isForeignKeyViolation(err):
		return ErrUnknownProject
	}
	return err
}

// CreateSprint inserts a sprint. It returns ErrConflict when the name is
// already taken within the sprint's project, ignoring case, and
// ErrUnknownProject when the project does not exist.
func (s *PostgresStore) CreateSprint(ctx context.Context, in SprintInput) (Sprint, error) {
	sp, err := scanSprint(s.pool.QueryRow(ctx, createSprintSQL, in.Name, in.StartDate, in.EndDate, in.ProjectID))
	if err != nil {
		return Sprint{}, sprintWriteError(err)
	}
	return sp, nil
}

// UpdateSprint replaces the sprint's name, dates and project. When the
// project is set or changed, it is added to every task in the sprint in
// the same transaction. It returns ErrNotFound, ErrConflict or
// ErrUnknownProject.
func (s *PostgresStore) UpdateSprint(ctx context.Context, id int64, in SprintInput) (Sprint, error) {
	var sp Sprint
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old *int64
		err := tx.QueryRow(ctx, lockSprintSQL, id).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, updateSprintSQL, in.Name, in.StartDate, in.EndDate, in.ProjectID, id); err != nil {
			return err
		}
		if in.ProjectID != nil && (old == nil || *old != *in.ProjectID) {
			if _, err := tx.Exec(ctx, addProjectToSprintTasksSQL, *in.ProjectID, id); err != nil {
				return err
			}
		}
		sp, err = scanSprint(tx.QueryRow(ctx, getSprintSQL, id))
		return err
	})
	if err != nil {
		return Sprint{}, sprintWriteError(err)
	}
	return sp, nil
}

// AddTasksToSprint adds the tasks to the sprint, and to the sprint's
// project if it has one, in one transaction; tasks already in the sprint
// are kept. It returns ErrNotFound for a missing sprint and
// *UnknownIDsError for missing tasks, changing nothing.
func (s *PostgresStore) AddTasksToSprint(ctx context.Context, sprintID int64, taskIDs []int64) (Sprint, error) {
	var sp Sprint
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var projectID *int64
		err := tx.QueryRow(ctx, shareSprintSQL, sprintID).Scan(&projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		rows, err := tx.Query(ctx, shareTasksSQL, taskIDs)
		if err != nil {
			return err
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		var unknown []int64
		for _, id := range taskIDs {
			if !slices.Contains(found, id) && !slices.Contains(unknown, id) {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			slices.Sort(unknown)
			return &UnknownIDsError{TaskIDs: unknown}
		}

		if _, err := tx.Exec(ctx, addSprintTasksSQL, sprintID, taskIDs); err != nil {
			return err
		}
		if projectID != nil {
			if _, err := tx.Exec(ctx, addProjectTasksSQL, *projectID, taskIDs); err != nil {
				return err
			}
		}
		sp, err = scanSprint(tx.QueryRow(ctx, getSprintSQL, sprintID))
		return err
	})
	if err != nil {
		return Sprint{}, err
	}
	return sp, nil
}

// RemoveTaskFromSprint removes the task from the sprint, keeping its
// project memberships. It returns ErrSprintNotFound, ErrNotFound (task) or
// ErrNotInSprint when there is nothing to remove.
func (s *PostgresStore) RemoveTaskFromSprint(ctx context.Context, sprintID, taskID int64) error {
	var sprintExists, taskExists, removed bool
	if err := s.pool.QueryRow(ctx, removeSprintTaskSQL, sprintID, taskID).Scan(&sprintExists, &taskExists, &removed); err != nil {
		return err
	}
	switch {
	case !sprintExists:
		return ErrSprintNotFound
	case !taskExists:
		return ErrNotFound
	case !removed:
		return ErrNotInSprint
	}
	return nil
}

// ListSprints returns all sprints ordered by start date, then id.
func (s *PostgresStore) ListSprints(ctx context.Context) ([]Sprint, error) {
	sprints, _, err := s.ListSprintsPage(ctx, SprintFilter{})
	return sprints, err
}

// ListSprintsPage returns f's page of the matching sprints, ordered by
// start date, then id, and the number of sprints matching f.
func (s *PostgresStore) ListSprintsPage(ctx context.Context, f SprintFilter) ([]Sprint, int, error) {
	var sprints []Sprint
	var total int
	err := readSnapshot(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		sprints, total, err = collectPage(ctx, tx, countSprintsSQL, listSprintsSQL, []any{f.ProjectID, f.Unassigned}, f.Page, scanSprint)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return sprints, total, nil
}

// GetSprint returns the sprint with the given id.
func (s *PostgresStore) GetSprint(ctx context.Context, id int64) (Sprint, error) {
	return scanSprint(s.pool.QueryRow(ctx, getSprintSQL, id))
}

// CreateProject inserts a project. It returns ErrConflict when the name is
// already taken, ignoring case.
func (s *PostgresStore) CreateProject(ctx context.Context, in ProjectInput) (Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx, createProjectSQL, in.Name))
	if isUniqueViolation(err) {
		return Project{}, ErrConflict
	}
	return p, err
}

// UpdateProject renames the project. It returns ErrNotFound or
// ErrConflict when the name is already taken, ignoring case.
func (s *PostgresStore) UpdateProject(ctx context.Context, id int64, in ProjectInput) (Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx, updateProjectSQL, in.Name, id))
	if isUniqueViolation(err) {
		return Project{}, ErrConflict
	}
	return p, err
}

// ListProjects returns all projects ordered by name ignoring case, then id.
func (s *PostgresStore) ListProjects(ctx context.Context) ([]Project, error) {
	projects, _, err := s.ListProjectsPage(ctx, ProjectFilter{})
	return projects, err
}

// ListProjectsPage returns f's page of the matching projects, ordered by
// name ignoring case, then id, and the number of projects matching f.
func (s *PostgresStore) ListProjectsPage(ctx context.Context, f ProjectFilter) ([]Project, int, error) {
	var projects []Project
	var total int
	err := readSnapshot(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		projects, total, err = collectPage(ctx, tx, countProjectsSQL, listProjectsSQL, []any{escapeLike(f.Q)}, f.Page, scanProject)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return projects, total, nil
}

// GetProject returns the project with the given id.
func (s *PostgresStore) GetProject(ctx context.Context, id int64) (Project, error) {
	return scanProject(s.pool.QueryRow(ctx, getProjectSQL, id))
}
