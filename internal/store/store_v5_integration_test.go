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

func TestEscapeLike(t *testing.T) {
	for in, want := range map[string]string{
		"":       "",
		"plain":  "plain",
		"50%":    `50\%`,
		"a_b":    `a\_b`,
		`c\d`:    `c\\d`,
		`%_\%_\`: `\%\_\\\%\_\\`,
		"日本_語%":  `日本\_語\%`,
	} {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPostgresStoreV5Integration covers sprint ownership by projects,
// automatic project membership, list filters and pagination. Set
// TEST_DATABASE_URL to a database with db/schema.sql applied to enable it.
func TestPostgresStoreV5Integration(t *testing.T) {
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
	newProject := func(name string) Project {
		t.Helper()
		p, err := s.CreateProject(ctx, ProjectInput{Name: name + "-" + suffix})
		if err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}
		projectIDs = append(projectIDs, p.ID)
		return p
	}
	newSprint := func(name, start string, project *int64) Sprint {
		t.Helper()
		sp, err := s.CreateSprint(ctx, SprintInput{Name: name + "-" + suffix, StartDate: day(start), EndDate: day(start).AddDate(0, 0, 13), ProjectID: project})
		if err != nil {
			t.Fatalf("create sprint %s: %v", name, err)
		}
		sprintIDs = append(sprintIDs, sp.ID)
		return sp
	}
	newTask := func(assignee, desc, status string, category *string, sprints, projects []int64) Task {
		t.Helper()
		tk, err := s.Create(ctx, TaskInput{Assignee: assignee, DueDate: day("2099-03-01"), Description: desc + " " + suffix, Status: status,
			Category: category, SprintIDs: sprints, ProjectIDs: projects})
		if err != nil {
			t.Fatalf("create task %s: %v", desc, err)
		}
		taskIDs = append(taskIDs, tk.ID)
		return tk
	}
	get := func(id int64) Task {
		t.Helper()
		tk, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %d: %v", id, err)
		}
		return tk
	}
	ids := func(tasks []Task) []int64 {
		out := []int64{}
		for _, tk := range tasks {
			out = append(out, tk.ID)
		}
		return out
	}
	eq := slices.Equal[[]int64]

	t.Run("schema", func(t *testing.T) {
		var oldIdx, newIdx bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'sprints_name_lower_idx'),
			EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'sprints_project_name_lower_idx')`).Scan(&oldIdx, &newIdx); err != nil {
			t.Fatal(err)
		}
		if oldIdx || !newIdx {
			t.Fatalf("indexes: global=%v per-project=%v", oldIdx, newIdx)
		}
	})

	auth, billing := newProject("Auth"), newProject("Billing")
	sa := newSprint("Sprint 1", "2099-01-01", &auth.ID)
	if sa.ProjectID == nil || *sa.ProjectID != auth.ID || sa.TaskCount != 0 {
		t.Fatalf("created sprint: %+v", sa)
	}
	sb := newSprint("Sprint 1", "2099-01-15", &billing.ID)
	legacy := newSprint("Legacy", "2098-12-01", nil)
	if legacy.ProjectID != nil {
		t.Fatalf("legacy sprint: %+v", legacy)
	}

	t.Run("sprint names unique per project", func(t *testing.T) {
		if _, err := s.CreateSprint(ctx, SprintInput{Name: "SPRINT 1-" + suffix, StartDate: day("2099-02-01"), EndDate: day("2099-02-02"), ProjectID: &auth.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("same project duplicate: %v", err)
		}
		if _, err := s.CreateSprint(ctx, SprintInput{Name: "legacy-" + suffix, StartDate: day("2099-02-01"), EndDate: day("2099-02-02")}); !errors.Is(err, ErrConflict) {
			t.Fatalf("legacy duplicate: %v", err)
		}
		missing := billing.ID + 1_000_000
		if _, err := s.CreateSprint(ctx, SprintInput{Name: "x-" + suffix, StartDate: day("2099-02-01"), EndDate: day("2099-02-02"), ProjectID: &missing}); !errors.Is(err, ErrUnknownProject) {
			t.Fatalf("unknown project: %v", err)
		}
		if _, err := s.UpdateSprint(ctx, sb.ID, SprintInput{Name: sa.Name, StartDate: sb.StartDate, EndDate: sb.EndDate, ProjectID: &auth.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("update into duplicate: %v", err)
		}
		if _, err := s.UpdateSprint(ctx, sb.ID, SprintInput{Name: sb.Name, StartDate: sb.StartDate, EndDate: sb.EndDate, ProjectID: &missing}); !errors.Is(err, ErrUnknownProject) {
			t.Fatalf("update unknown project: %v", err)
		}
		if _, err := s.UpdateSprint(ctx, sb.ID+1_000_000, SprintInput{Name: "x", StartDate: sb.StartDate, EndDate: sb.EndDate, ProjectID: &auth.ID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update missing sprint: %v", err)
		}
		if got, err := s.GetSprint(ctx, sb.ID); err != nil || *got.ProjectID != billing.ID || got.Name != sb.Name {
			t.Fatalf("rejected updates changed sprint: %+v %v", got, err)
		}
	})

	finance, sales := "finance", "sales"
	inA := newTask("Alice", "tagged in A", "not_started", &finance, []int64{sa.ID}, nil)
	if !eq(inA.ProjectIDs, []int64{auth.ID}) {
		t.Fatalf("create with sprint must add its project: %+v", inA)
	}
	inLegacy := newTask("Bob", "legacy member", "blocked", nil, []int64{legacy.ID}, []int64{billing.ID})
	if !eq(inLegacy.ProjectIDs, []int64{billing.ID}) {
		t.Fatalf("legacy sprint added a project: %+v", inLegacy)
	}
	loose := newTask("Carol", "loose", "done", &sales, nil, nil)

	t.Run("PUT sprintIds adds projects, untag keeps them", func(t *testing.T) {
		in := TaskInput{Assignee: "Carol", DueDate: day("2099-03-01"), Description: loose.Description, Status: "done",
			SprintIDs: []int64{sb.ID, legacy.ID}, ProjectIDs: []int64{}}
		upd, err := s.Update(ctx, loose.ID, in)
		if err != nil || !eq(upd.SprintIDs, []int64{sb.ID, legacy.ID}) || !eq(upd.ProjectIDs, []int64{billing.ID}) {
			t.Fatalf("PUT sprintIds: %+v %v", upd, err)
		}
		in.SprintIDs, in.ProjectIDs = nil, []int64{}
		if upd, err = s.Update(ctx, loose.ID, in); err != nil || !eq(upd.ProjectIDs, []int64{billing.ID}) {
			t.Fatalf("projectIds-only PUT must keep sprint projects: %+v %v", upd, err)
		}
		in.SprintIDs, in.ProjectIDs = []int64{}, nil
		if upd, err = s.Update(ctx, loose.ID, in); err != nil || len(upd.SprintIDs) != 0 || !eq(upd.ProjectIDs, []int64{billing.ID}) {
			t.Fatalf("untag via PUT: %+v %v", upd, err)
		}
	})

	t.Run("add and remove sprint tasks", func(t *testing.T) {
		sp, err := s.AddTasksToSprint(ctx, sa.ID, []int64{loose.ID, inA.ID})
		if err != nil || sp.TaskCount != 2 {
			t.Fatalf("add: %+v %v", sp, err)
		}
		if got := get(loose.ID); !eq(got.SprintIDs, []int64{sa.ID}) || !eq(got.ProjectIDs, []int64{auth.ID, billing.ID}) {
			t.Fatalf("added task: %+v", got)
		}
		if sp, err = s.AddTasksToSprint(ctx, sa.ID, []int64{loose.ID}); err != nil || sp.TaskCount != 2 {
			t.Fatalf("idempotent add: %+v %v", sp, err)
		}

		missing := loose.ID + 1_000_000
		_, err = s.AddTasksToSprint(ctx, sb.ID, []int64{inA.ID, missing})
		var unknown *UnknownIDsError
		if !errors.As(err, &unknown) || !eq(unknown.TaskIDs, []int64{missing}) {
			t.Fatalf("unknown task: %v", err)
		}
		if got := get(inA.ID); !eq(got.SprintIDs, []int64{sa.ID}) || !eq(got.ProjectIDs, []int64{auth.ID}) {
			t.Fatalf("rejected add was not rolled back: %+v", got)
		}
		if _, err := s.AddTasksToSprint(ctx, sb.ID+1_000_000, []int64{inA.ID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing sprint: %v", err)
		}

		if err := s.RemoveTaskFromSprint(ctx, sa.ID, loose.ID); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if got := get(loose.ID); len(got.SprintIDs) != 0 || !eq(got.ProjectIDs, []int64{auth.ID, billing.ID}) {
			t.Fatalf("untag must keep projects: %+v", got)
		}
		for _, tc := range []struct {
			sprint, task int64
			want         error
		}{
			{sa.ID, loose.ID, ErrNotInSprint},
			{sa.ID + 1_000_000, loose.ID, ErrSprintNotFound},
			{sa.ID, missing, ErrNotFound},
		} {
			if err := s.RemoveTaskFromSprint(ctx, tc.sprint, tc.task); !errors.Is(err, tc.want) {
				t.Fatalf("remove %d/%d: %v, want %v", tc.sprint, tc.task, err, tc.want)
			}
		}
	})

	t.Run("setting a legacy sprint's project adds it to its tasks", func(t *testing.T) {
		sp, err := s.UpdateSprint(ctx, legacy.ID, SprintInput{Name: "Legacy renamed-" + suffix, StartDate: day("2098-12-02"), EndDate: day("2098-12-20"), ProjectID: &auth.ID})
		if err != nil || sp.ProjectID == nil || *sp.ProjectID != auth.ID || sp.Name != "Legacy renamed-"+suffix || sp.TaskCount != 1 ||
			sp.StartDate.Format("2006-01-02") != "2098-12-02" {
			t.Fatalf("update legacy: %+v %v", sp, err)
		}
		if got := get(inLegacy.ID); !eq(got.ProjectIDs, []int64{auth.ID, billing.ID}) {
			t.Fatalf("project not added: %+v", got)
		}
		if _, err := s.UpdateSprint(ctx, legacy.ID, SprintInput{Name: sp.Name, StartDate: sp.StartDate, EndDate: sp.EndDate, ProjectID: &billing.ID}); err != nil {
			t.Fatal(err)
		}
		if got := get(inLegacy.ID); !eq(got.ProjectIDs, []int64{auth.ID, billing.ID}) {
			t.Fatalf("changing project must not remove the old one: %+v", got)
		}
		if p, err := s.GetProject(ctx, billing.ID); err != nil || p.SprintCount != 2 || p.TaskCount != 2 {
			t.Fatalf("billing counts: %+v %v", p, err)
		}
	})

	t.Run("sprint list filters and pagination", func(t *testing.T) {
		sprints, total, err := s.ListSprintsPage(ctx, SprintFilter{ProjectID: billing.ID})
		if err != nil || total != 2 || len(sprints) != 2 || sprints[0].ID != legacy.ID || sprints[1].ID != sb.ID {
			t.Fatalf("billing sprints: %+v %d %v", sprints, total, err)
		}
		sprints, total, err = s.ListSprintsPage(ctx, SprintFilter{ProjectID: billing.ID, Page: Page{Limit: 1, Offset: 1}})
		if err != nil || total != 2 || len(sprints) != 1 || sprints[0].ID != sb.ID {
			t.Fatalf("billing page 2: %+v %d %v", sprints, total, err)
		}
		sprints, total, err = s.ListSprintsPage(ctx, SprintFilter{ProjectID: billing.ID, Page: Page{Limit: 10, Offset: 10}})
		if err != nil || total != 2 || sprints == nil || len(sprints) != 0 {
			t.Fatalf("past end: %+v %d %v", sprints, total, err)
		}
		unassigned, _, err := s.ListSprintsPage(ctx, SprintFilter{Unassigned: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, sp := range unassigned {
			if sp.ProjectID != nil || slices.Contains(sprintIDs, sp.ID) {
				t.Fatalf("unassigned list: %+v", sp)
			}
		}
	})

	t.Run("task filters, search escaping and pagination", func(t *testing.T) {
		p := newProject("Search")
		var unclassified *string
		pct := newTask("Dana", "Give 50% off", "in_progress", &sales, nil, []int64{p.ID})
		five := newTask("Dana", "Give 500 off", "in_progress", &sales, nil, []int64{p.ID})
		under := newTask("Eve_Q", "underscore", "not_started", unclassified, nil, []int64{p.ID})
		notUnder := newTask("EveXQ", "no underscore", "not_started", unclassified, nil, []int64{p.ID})
		slash := newTask("Fay", `back\slash`, "done", &finance, []int64{sa.ID}, []int64{p.ID})
		noSlash := newTask("Fay", "backslash", "done", &finance, nil, []int64{p.ID})

		list := func(f TaskFilter) ([]int64, int) {
			t.Helper()
			f.ProjectID = p.ID
			tasks, total, err := s.ListTasksPage(ctx, f)
			if err != nil {
				t.Fatalf("list %+v: %v", f, err)
			}
			return ids(tasks), total
		}
		all := []int64{pct.ID, five.ID, under.ID, notUnder.ID, slash.ID, noSlash.ID}
		for _, tc := range []struct {
			f    TaskFilter
			want []int64
		}{
			{TaskFilter{}, all},
			{TaskFilter{Q: "50%"}, []int64{pct.ID}},
			{TaskFilter{Q: "eve_"}, []int64{under.ID}},
			{TaskFilter{Q: `k\s`}, []int64{slash.ID}},
			{TaskFilter{Q: "DANA"}, []int64{pct.ID, five.ID}},
			{TaskFilter{Q: "'; DROP TABLE tasks; --"}, []int64{}},
			{TaskFilter{Status: "done"}, []int64{slash.ID, noSlash.ID}},
			{TaskFilter{Category: Unclassified}, []int64{under.ID, notUnder.ID}},
			{TaskFilter{Category: "sales", Status: "in_progress"}, []int64{pct.ID, five.ID}},
			{TaskFilter{ExcludeSprintID: sa.ID}, []int64{pct.ID, five.ID, under.ID, notUnder.ID, noSlash.ID}},
			{TaskFilter{SprintID: sa.ID}, []int64{slash.ID}},
		} {
			if got, total := list(tc.f); !eq(got, tc.want) || total != len(tc.want) {
				t.Fatalf("filter %+v = %v (total %d), want %v", tc.f, got, total, tc.want)
			}
		}
		for _, tc := range []struct {
			page  Page
			want  []int64
			total int
		}{
			{Page{Limit: 4}, all[:4], 6},
			{Page{Limit: 4, Offset: 4}, all[4:], 6},
			{Page{Limit: 4, Offset: 8}, []int64{}, 6},
		} {
			if got, total := list(TaskFilter{Page: tc.page}); !eq(got, tc.want) || total != tc.total {
				t.Fatalf("page %+v = %v (total %d)", tc.page, got, total)
			}
		}
		if got, total := list(TaskFilter{Status: "not_started", Page: Page{Limit: 1, Offset: 1}}); !eq(got, []int64{notUnder.ID}) || total != 2 {
			t.Fatalf("filtered page = %v (total %d)", got, total)
		}

		projects, total, err := s.ListProjectsPage(ctx, ProjectFilter{Q: "search-" + suffix})
		if err != nil || total != 1 || len(projects) != 1 || projects[0].ID != p.ID || projects[0].TaskCount != 6 {
			t.Fatalf("project search: %+v %d %v", projects, total, err)
		}
		pctProject := newProject("100%_X")
		if projects, _, err = s.ListProjectsPage(ctx, ProjectFilter{Q: "100%_X-" + suffix}); err != nil || len(projects) != 1 || projects[0].ID != pctProject.ID {
			t.Fatalf("project search escaping: %+v %v", projects, err)
		}
		if projects, _, err = s.ListProjectsPage(ctx, ProjectFilter{Q: "100__x-" + suffix}); err != nil || len(projects) != 0 {
			t.Fatalf("underscore must be literal: %+v %v", projects, err)
		}
	})

	t.Run("comment pagination", func(t *testing.T) {
		for i := 1; i <= 3; i++ {
			if _, err := s.CreateComment(ctx, inA.ID, CommentInput{Author: "A", Body: "c" + strconv.Itoa(i)}); err != nil {
				t.Fatal(err)
			}
		}
		comments, total, err := s.ListCommentsPage(ctx, inA.ID, Page{Limit: 2, Offset: 2})
		if err != nil || total != 3 || len(comments) != 1 || comments[0].Body != "c3" {
			t.Fatalf("comments page: %+v %d %v", comments, total, err)
		}
		if _, _, err := s.ListCommentsPage(ctx, loose.ID+1_000_000, Page{Limit: 2}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing task: %v", err)
		}
	})

	t.Run("project rename", func(t *testing.T) {
		p, err := s.UpdateProject(ctx, auth.ID, ProjectInput{Name: "Identity-" + suffix})
		if err != nil || p.Name != "Identity-"+suffix || p.SprintCount != 1 || p.TaskCount != 4 {
			t.Fatalf("rename: %+v %v", p, err)
		}
		if _, err := s.UpdateProject(ctx, auth.ID, ProjectInput{Name: strings.ToUpper(billing.Name)}); !errors.Is(err, ErrConflict) {
			t.Fatalf("rename conflict: %v", err)
		}
		if _, err := s.UpdateProject(ctx, billing.ID+1_000_000, ProjectInput{Name: "x"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rename missing: %v", err)
		}
	})
}
