package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
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

// TestPostgresStoreMembershipsIntegration covers categories, sprints,
// projects and task memberships. Set TEST_DATABASE_URL to enable it.
func TestPostgresStoreMembershipsIntegration(t *testing.T) {
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
	var taskIDs, sprintIDs, projectIDs []int64
	t.Cleanup(func() {
		for _, c := range []struct {
			sql string
			ids []int64
		}{
			{"DELETE FROM tasks WHERE id = ANY($1)", taskIDs},
			{"DELETE FROM sprints WHERE id = ANY($1)", sprintIDs},
			{"DELETE FROM projects WHERE id = ANY($1)", projectIDs},
		} {
			if _, err := pool.Exec(context.Background(), c.sql, c.ids); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	day := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	newSprint := func(name, start, end string) Sprint {
		t.Helper()
		sp, err := s.CreateSprint(ctx, SprintInput{Name: name + "-" + suffix, StartDate: day(start), EndDate: day(end)})
		if err != nil {
			t.Fatalf("create sprint %s: %v", name, err)
		}
		sprintIDs = append(sprintIDs, sp.ID)
		return sp
	}
	newProject := func(name string) Project {
		t.Helper()
		p, err := s.CreateProject(ctx, ProjectInput{Name: name + "-" + suffix})
		if err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}
		projectIDs = append(projectIDs, p.ID)
		return p
	}
	newTask := func(desc string, category *string, sprints, projects []int64) Task {
		t.Helper()
		tk, err := s.Create(ctx, TaskInput{Assignee: "Alice", DueDate: day("2099-01-01"), Description: desc, Status: "not_started",
			Category: category, SprintIDs: sprints, ProjectIDs: projects})
		if err != nil {
			t.Fatalf("create task %s: %v", desc, err)
		}
		taskIDs = append(taskIDs, tk.ID)
		return tk
	}
	eq := func(a, b []int64) bool { return slices.Equal(a, b) }
	finance, sales := "finance", "sales"

	s1 := newSprint("S1", "2099-01-01", "2099-01-14")
	s2 := newSprint("S2", "2099-01-15", "2099-01-28")
	auth := newProject("Auth")
	billing := newProject("Billing")
	if s1.TaskCount != 0 || s1.StartDate.Format("2006-01-02") != "2099-01-01" || s1.CreatedAt.IsZero() || auth.TaskCount != 0 {
		t.Fatalf("created: %+v %+v", s1, auth)
	}

	if _, err := s.CreateSprint(ctx, SprintInput{Name: strings.ToUpper("s1-" + suffix), StartDate: day("2099-02-01"), EndDate: day("2099-02-02")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate sprint: %v", err)
	}
	if _, err := s.CreateProject(ctx, ProjectInput{Name: "auth-" + suffix}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate project: %v", err)
	}

	plain := newTask("plain", nil, nil, nil)
	if plain.Category != nil || plain.SprintIDs == nil || len(plain.SprintIDs) != 0 || plain.ProjectIDs == nil || len(plain.ProjectIDs) != 0 {
		t.Fatalf("plain task: %+v", plain)
	}

	spill := newTask("spill-over", &finance, []int64{s2.ID, s1.ID, s1.ID}, []int64{auth.ID})
	if *spill.Category != "finance" || !eq(spill.SprintIDs, []int64{s1.ID, s2.ID}) || !eq(spill.ProjectIDs, []int64{auth.ID}) {
		t.Fatalf("spill-over task: %+v", spill)
	}
	shared := newTask("shared", nil, []int64{s1.ID}, []int64{billing.ID, auth.ID})
	if !eq(shared.ProjectIDs, []int64{auth.ID, billing.ID}) {
		t.Fatalf("shared task: %+v", shared)
	}
	if got, err := s.Get(ctx, spill.ID); err != nil || !eq(got.SprintIDs, spill.SprintIDs) || *got.Category != "finance" {
		t.Fatalf("get spill-over: %+v %v", got, err)
	}

	var before int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tasks").Scan(&before); err != nil {
		t.Fatal(err)
	}
	missingSprint, missingProject := s2.ID+1_000_000, billing.ID+1_000_000
	_, err = s.Create(ctx, TaskInput{Assignee: "A", DueDate: day("2099-01-01"), Description: "orphan?", Status: "not_started",
		SprintIDs: []int64{s1.ID, missingSprint}, ProjectIDs: []int64{missingProject}})
	var unknown *UnknownIDsError
	if !errors.As(err, &unknown) || !eq(unknown.SprintIDs, []int64{missingSprint}) || !eq(unknown.ProjectIDs, []int64{missingProject}) {
		t.Fatalf("unknown ids on create: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tasks").Scan(&after); err != nil || after != before {
		t.Fatalf("rejected create left a task: before=%d after=%d %v", before, after, err)
	}

	list := func(f TaskFilter) []int64 {
		t.Helper()
		tasks, err := s.ListFiltered(ctx, f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		ids := []int64{}
		for _, tk := range tasks {
			ids = append(ids, tk.ID)
		}
		return ids
	}
	for _, tc := range []struct {
		f    TaskFilter
		want []int64
	}{
		{TaskFilter{SprintID: s1.ID}, []int64{spill.ID, shared.ID}},
		{TaskFilter{SprintID: s2.ID}, []int64{spill.ID}},
		{TaskFilter{ProjectID: auth.ID}, []int64{spill.ID, shared.ID}},
		{TaskFilter{ProjectID: billing.ID}, []int64{shared.ID}},
		{TaskFilter{SprintID: s2.ID, ProjectID: billing.ID}, []int64{}},
		{TaskFilter{SprintID: missingSprint}, []int64{}},
	} {
		if got := list(tc.f); !eq(got, tc.want) {
			t.Fatalf("filter %+v = %v, want %v", tc.f, got, tc.want)
		}
	}
	if got, err := s.GetSprint(ctx, s1.ID); err != nil || got.TaskCount != 2 || got.Name != "S1-"+suffix {
		t.Fatalf("get sprint: %+v %v", got, err)
	}
	if got, err := s.GetProject(ctx, auth.ID); err != nil || got.TaskCount != 2 {
		t.Fatalf("get project: %+v %v", got, err)
	}

	edit := TaskInput{Assignee: "Bob", DueDate: day("2099-01-02"), Description: "moved", Status: "blocked"}
	upd, err := s.Update(ctx, spill.ID, edit)
	if err != nil || upd.Assignee != "Bob" || *upd.Category != "finance" || !eq(upd.SprintIDs, []int64{s1.ID, s2.ID}) || !eq(upd.ProjectIDs, []int64{auth.ID}) {
		t.Fatalf("update without category/ids must preserve them: %+v %v", upd, err)
	}

	move := edit
	move.SprintIDs = []int64{s2.ID}
	move.Category = &sales
	upd, err = s.Update(ctx, spill.ID, move)
	if err != nil || !eq(upd.SprintIDs, []int64{s2.ID}) || !eq(upd.ProjectIDs, []int64{auth.ID}) || *upd.Category != "sales" {
		t.Fatalf("move: %+v %v", upd, err)
	}
	if got := list(TaskFilter{SprintID: s1.ID}); !eq(got, []int64{shared.ID}) {
		t.Fatalf("after move S1 = %v", got)
	}

	cleared := edit
	cleared.ProjectIDs = []int64{}
	if upd, err = s.Update(ctx, spill.ID, cleared); err != nil || len(upd.ProjectIDs) != 0 || !eq(upd.SprintIDs, []int64{s2.ID}) {
		t.Fatalf("clear projects: %+v %v", upd, err)
	}

	bad := edit
	bad.Description = "must not stick"
	bad.SprintIDs = []int64{missingSprint}
	if _, err := s.Update(ctx, spill.ID, bad); !errors.As(err, &unknown) {
		t.Fatalf("unknown ids on update: %v", err)
	}
	if got, err := s.Get(ctx, spill.ID); err != nil || got.Description != "moved" || !eq(got.SprintIDs, []int64{s2.ID}) {
		t.Fatalf("rejected update was not rolled back: %+v %v", got, err)
	}
	missingTask := edit
	missingTask.SprintIDs = []int64{s1.ID}
	if _, err := s.Update(ctx, shared.ID+1_000_000, missingTask); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing task: %v", err)
	}

	sprints, err := s.ListSprints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[int64]int{}
	for i, sp := range sprints {
		pos[sp.ID] = i
	}
	if pos[s1.ID] > pos[s2.ID] {
		t.Fatalf("sprint order: %+v", sprints)
	}
	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ppos := map[int64]int{}
	for i, p := range projects {
		ppos[p.ID] = i
	}
	if ppos[auth.ID] > ppos[billing.ID] {
		t.Fatalf("project order: %+v", projects)
	}
	if _, err := s.GetSprint(ctx, missingSprint); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing sprint: %v", err)
	}
	if _, err := s.GetProject(ctx, missingProject); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing project: %v", err)
	}

	if err := s.Delete(ctx, shared.ID); err != nil {
		t.Fatal(err)
	}
	var memberships int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM sprint_tasks WHERE task_id = $1) + (SELECT count(*) FROM project_tasks WHERE task_id = $1)", shared.ID).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatalf("memberships not cascaded: %d %v", memberships, err)
	}
	if got, err := s.GetProject(ctx, billing.ID); err != nil || got.TaskCount != 0 {
		t.Fatalf("billing after delete: %+v %v", got, err)
	}
	if _, err := s.Update(ctx, plain.ID, TaskInput{Assignee: "x", DueDate: day("2099-01-01"), Description: "x", Status: "done", Category: strPtr("bogus")}); err == nil {
		t.Fatalf("category check constraint not enforced")
	}
}

func strPtr(s string) *string { return &s }
