package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"taskmanager/backend/internal/store"
)

type fakeStore struct {
	mu            sync.Mutex
	nextID        int64
	nextCommentID int64
	nextSprintID  int64
	nextProjectID int64
	tasks         map[int64]store.Task
	comments      map[int64][]store.Comment
	sprints       map[int64]store.Sprint
	projects      map[int64]store.Project
	err           error
}

func newFakeStore() *fakeStore {
	return &fakeStore{nextID: 1, nextCommentID: 1, nextSprintID: 1, nextProjectID: 1,
		tasks: map[int64]store.Task{}, comments: map[int64][]store.Comment{},
		sprints: map[int64]store.Sprint{}, projects: map[int64]store.Project{}}
}

func (f *fakeStore) List(context.Context) ([]store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].DueDate.Equal(out[j].DueDate) {
			return out[i].DueDate.Before(out[j].DueDate)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeStore) ListFiltered(ctx context.Context, filter store.TaskFilter) ([]store.Task, error) {
	all, err := f.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []store.Task{}
	for _, t := range all {
		if (filter.SprintID == 0 || slices.Contains(t.SprintIDs, filter.SprintID)) &&
			(filter.ProjectID == 0 || slices.Contains(t.ProjectIDs, filter.ProjectID)) {
			out = append(out, t)
		}
	}
	return out, nil
}

// paginate mirrors LIMIT/OFFSET; a zero Limit keeps everything.
func paginate[T any](items []T, p store.Page) []T {
	if p.Limit == 0 {
		return items
	}
	if p.Offset >= int64(len(items)) {
		return []T{}
	}
	return items[p.Offset:min(p.Offset+p.Limit, int64(len(items)))]
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func (f *fakeStore) ListTasksPage(ctx context.Context, filter store.TaskFilter) ([]store.Task, int, error) {
	all, err := f.ListFiltered(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	out := []store.Task{}
	for _, t := range all {
		category := store.Unclassified
		if t.Category != nil {
			category = *t.Category
		}
		if (filter.Status == "" || t.Status == filter.Status) &&
			(filter.Category == "" || category == filter.Category) &&
			(filter.Q == "" || containsFold(t.Assignee, filter.Q) || containsFold(t.Description, filter.Q)) &&
			(filter.ExcludeSprintID == 0 || !slices.Contains(t.SprintIDs, filter.ExcludeSprintID)) {
			out = append(out, t)
		}
	}
	return paginate(out, filter.Page), len(out), nil
}

// withSprintProjects mirrors the Postgres store: a task in a sprint that
// belongs to a project is also in that project.
func (f *fakeStore) withSprintProjects(projectIDs, sprintIDs []int64) []int64 {
	out := append([]int64{}, projectIDs...)
	for _, id := range sprintIDs {
		if p := f.sprints[id].ProjectID; p != nil {
			out = append(out, *p)
		}
	}
	return sortedIDs(out)
}

// checkIDs mirrors the Postgres store: unknown ids fail the whole write.
func (f *fakeStore) checkIDs(in store.TaskInput) error {
	var unknown store.UnknownIDsError
	for _, id := range in.SprintIDs {
		if _, ok := f.sprints[id]; !ok {
			unknown.SprintIDs = append(unknown.SprintIDs, id)
		}
	}
	for _, id := range in.ProjectIDs {
		if _, ok := f.projects[id]; !ok {
			unknown.ProjectIDs = append(unknown.ProjectIDs, id)
		}
	}
	if len(unknown.SprintIDs) > 0 || len(unknown.ProjectIDs) > 0 {
		return &unknown
	}
	return nil
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64{}, ids...)
	slices.Sort(out)
	return slices.Compact(out)
}

func (f *fakeStore) Get(_ context.Context, id int64) (store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Task{}, f.err
	}
	t, ok := f.tasks[id]
	if !ok {
		return store.Task{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) Create(_ context.Context, in store.TaskInput) (store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Task{}, f.err
	}
	if err := f.checkIDs(in); err != nil {
		return store.Task{}, err
	}
	now := time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
	t := store.Task{ID: f.nextID, Assignee: in.Assignee, DueDate: in.DueDate, Description: in.Description, Status: in.Status,
		Category: in.Category, SprintIDs: sortedIDs(in.SprintIDs), ProjectIDs: f.withSprintProjects(in.ProjectIDs, in.SprintIDs),
		CreatedAt: now, UpdatedAt: now}
	f.tasks[t.ID] = t
	f.nextID++
	return t, nil
}

func (f *fakeStore) Update(_ context.Context, id int64, in store.TaskInput) (store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Task{}, f.err
	}
	t, ok := f.tasks[id]
	if !ok {
		return store.Task{}, store.ErrNotFound
	}
	if err := f.checkIDs(in); err != nil {
		return store.Task{}, err
	}
	t.Assignee, t.DueDate, t.Description, t.Status = in.Assignee, in.DueDate, in.Description, in.Status
	if in.Category != nil {
		t.Category = in.Category
	}
	if in.SprintIDs != nil {
		t.SprintIDs = sortedIDs(in.SprintIDs)
	}
	if in.ProjectIDs != nil {
		t.ProjectIDs = sortedIDs(in.ProjectIDs)
	}
	if in.SprintIDs != nil || in.ProjectIDs != nil {
		t.ProjectIDs = f.withSprintProjects(t.ProjectIDs, t.SprintIDs)
	}
	t.UpdatedAt = t.UpdatedAt.Add(time.Minute)
	f.tasks[id] = t
	return t, nil
}

func (f *fakeStore) Delete(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.tasks[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.tasks, id)
	delete(f.comments, id)
	return nil
}

func (f *fakeStore) ListComments(_ context.Context, taskID int64) ([]store.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.tasks[taskID]; !ok {
		return nil, store.ErrNotFound
	}
	return append([]store.Comment{}, f.comments[taskID]...), nil
}

func (f *fakeStore) ListCommentsPage(ctx context.Context, taskID int64, p store.Page) ([]store.Comment, int, error) {
	all, err := f.ListComments(ctx, taskID)
	if err != nil {
		return nil, 0, err
	}
	return paginate(all, p), len(all), nil
}

func (f *fakeStore) CreateComment(_ context.Context, taskID int64, in store.CommentInput) (store.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Comment{}, f.err
	}
	if _, ok := f.tasks[taskID]; !ok {
		return store.Comment{}, store.ErrNotFound
	}
	created := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC).Add(time.Duration(f.nextCommentID) * time.Second)
	c := store.Comment{ID: f.nextCommentID, TaskID: taskID, Author: in.Author, Body: in.Body, CreatedAt: created}
	f.comments[taskID] = append(f.comments[taskID], c)
	f.nextCommentID++
	return c, nil
}

func sameProject(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// checkSprint mirrors the Postgres constraints on a sprint write by sprint
// self (0 for a new sprint).
func (f *fakeStore) checkSprint(self int64, in store.SprintInput) error {
	if in.ProjectID != nil {
		if _, ok := f.projects[*in.ProjectID]; !ok {
			return store.ErrUnknownProject
		}
	}
	for _, s := range f.sprints {
		if s.ID != self && sameProject(s.ProjectID, in.ProjectID) && strings.EqualFold(s.Name, in.Name) {
			return store.ErrConflict
		}
	}
	return nil
}

func (f *fakeStore) CreateSprint(_ context.Context, in store.SprintInput) (store.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Sprint{}, f.err
	}
	if err := f.checkSprint(0, in); err != nil {
		return store.Sprint{}, err
	}
	return f.addSprint(in), nil
}

// addSprint stores a sprint without checks; a nil ProjectID makes a legacy
// (pre-v5) sprint.
func (f *fakeStore) addSprint(in store.SprintInput) store.Sprint {
	s := store.Sprint{ID: f.nextSprintID, Name: in.Name, StartDate: in.StartDate, EndDate: in.EndDate, ProjectID: in.ProjectID,
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	f.sprints[s.ID] = s
	f.nextSprintID++
	return s
}

func (f *fakeStore) UpdateSprint(_ context.Context, id int64, in store.SprintInput) (store.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Sprint{}, f.err
	}
	s, ok := f.sprints[id]
	if !ok {
		return store.Sprint{}, store.ErrNotFound
	}
	if err := f.checkSprint(id, in); err != nil {
		return store.Sprint{}, err
	}
	changed := in.ProjectID != nil && !sameProject(s.ProjectID, in.ProjectID)
	s.Name, s.StartDate, s.EndDate, s.ProjectID = in.Name, in.StartDate, in.EndDate, in.ProjectID
	f.sprints[id] = s
	if changed {
		for tid, t := range f.tasks {
			if slices.Contains(t.SprintIDs, id) {
				t.ProjectIDs = sortedIDs(append(t.ProjectIDs, *in.ProjectID))
				f.tasks[tid] = t
			}
		}
	}
	return f.withSprintCount(s), nil
}

func (f *fakeStore) AddTasksToSprint(_ context.Context, sprintID int64, taskIDs []int64) (store.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Sprint{}, f.err
	}
	s, ok := f.sprints[sprintID]
	if !ok {
		return store.Sprint{}, store.ErrNotFound
	}
	var unknown []int64
	for _, id := range taskIDs {
		if _, ok := f.tasks[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return store.Sprint{}, &store.UnknownIDsError{TaskIDs: sortedIDs(unknown)}
	}
	for _, id := range taskIDs {
		t := f.tasks[id]
		t.SprintIDs = sortedIDs(append(t.SprintIDs, sprintID))
		t.ProjectIDs = f.withSprintProjects(t.ProjectIDs, []int64{sprintID})
		f.tasks[id] = t
	}
	return f.withSprintCount(s), nil
}

func (f *fakeStore) RemoveTaskFromSprint(_ context.Context, sprintID, taskID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if _, ok := f.sprints[sprintID]; !ok {
		return store.ErrSprintNotFound
	}
	t, ok := f.tasks[taskID]
	if !ok {
		return store.ErrNotFound
	}
	i := slices.Index(t.SprintIDs, sprintID)
	if i < 0 {
		return store.ErrNotInSprint
	}
	t.SprintIDs = slices.Delete(slices.Clone(t.SprintIDs), i, i+1)
	f.tasks[taskID] = t
	return nil
}

func (f *fakeStore) withSprintCount(s store.Sprint) store.Sprint {
	for _, t := range f.tasks {
		if slices.Contains(t.SprintIDs, s.ID) {
			s.TaskCount++
		}
	}
	return s
}

func (f *fakeStore) ListSprints(context.Context) ([]store.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := []store.Sprint{}
	for _, s := range f.sprints {
		out = append(out, f.withSprintCount(s))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartDate.Equal(out[j].StartDate) {
			return out[i].StartDate.Before(out[j].StartDate)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeStore) ListSprintsPage(ctx context.Context, filter store.SprintFilter) ([]store.Sprint, int, error) {
	all, err := f.ListSprints(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []store.Sprint{}
	for _, s := range all {
		if (filter.ProjectID == 0 || (s.ProjectID != nil && *s.ProjectID == filter.ProjectID)) &&
			(!filter.Unassigned || s.ProjectID == nil) {
			out = append(out, s)
		}
	}
	return paginate(out, filter.Page), len(out), nil
}

func (f *fakeStore) GetSprint(_ context.Context, id int64) (store.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Sprint{}, f.err
	}
	s, ok := f.sprints[id]
	if !ok {
		return store.Sprint{}, store.ErrNotFound
	}
	return f.withSprintCount(s), nil
}

func (f *fakeStore) CreateProject(_ context.Context, in store.ProjectInput) (store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Project{}, f.err
	}
	for _, p := range f.projects {
		if strings.EqualFold(p.Name, in.Name) {
			return store.Project{}, store.ErrConflict
		}
	}
	p := store.Project{ID: f.nextProjectID, Name: in.Name, CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	f.projects[p.ID] = p
	f.nextProjectID++
	return p, nil
}

func (f *fakeStore) UpdateProject(_ context.Context, id int64, in store.ProjectInput) (store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Project{}, f.err
	}
	p, ok := f.projects[id]
	if !ok {
		return store.Project{}, store.ErrNotFound
	}
	for _, other := range f.projects {
		if other.ID != id && strings.EqualFold(other.Name, in.Name) {
			return store.Project{}, store.ErrConflict
		}
	}
	p.Name = in.Name
	f.projects[id] = p
	return f.withProjectCount(p), nil
}

func (f *fakeStore) withProjectCount(p store.Project) store.Project {
	for _, t := range f.tasks {
		if slices.Contains(t.ProjectIDs, p.ID) {
			p.TaskCount++
		}
	}
	for _, s := range f.sprints {
		if s.ProjectID != nil && *s.ProjectID == p.ID {
			p.SprintCount++
		}
	}
	return p
}

func (f *fakeStore) ListProjects(context.Context) ([]store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := []store.Project{}
	for _, p := range f.projects {
		out = append(out, f.withProjectCount(p))
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeStore) ListProjectsPage(ctx context.Context, filter store.ProjectFilter) ([]store.Project, int, error) {
	all, err := f.ListProjects(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := []store.Project{}
	for _, p := range all {
		if filter.Q == "" || containsFold(p.Name, filter.Q) {
			out = append(out, p)
		}
	}
	return paginate(out, filter.Page), len(out), nil
}

func (f *fakeStore) GetProject(_ context.Context, id int64) (store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Project{}, f.err
	}
	p, ok := f.projects[id]
	if !ok {
		return store.Project{}, store.ErrNotFound
	}
	return f.withProjectCount(p), nil
}

const testOrigin = "http://localhost:3000"

func newTestHandler(s TaskStore) http.Handler {
	return NewHandler(s, testOrigin, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, want, rec.Body.String())
	}
}

func TestCRUDLifecycle(t *testing.T) {
	h := newTestHandler(newFakeStore())

	rec := do(t, h, "GET", "/api/tasks", "")
	expectStatus(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("empty list body = %q, want []", got)
	}

	rec = do(t, h, "POST", "/api/tasks", `{"assignee":" Alice ","date":"2026-10-31","description":"Write docs"}`)
	expectStatus(t, rec, http.StatusCreated)
	created := decode[taskResponse](t, rec)
	want := taskResponse{ID: 1, Assignee: "Alice", Date: "2026-10-31", Description: "Write docs", Status: "not_started",
		CreatedAt: "2026-10-20T13:00:00Z", UpdatedAt: "2026-10-20T13:00:00Z"}
	if created != want {
		t.Fatalf("created = %+v, want %+v", created, want)
	}

	do(t, h, "POST", "/api/tasks", `{"assignee":"Bob","date":"2026-01-15","description":"Earlier"}`)

	rec = do(t, h, "GET", "/api/tasks", "")
	expectStatus(t, rec, http.StatusOK)
	list := decode[[]taskResponse](t, rec)
	if len(list) != 2 || list[0].ID != 2 || list[1].ID != 1 {
		t.Fatalf("list order wrong: %+v", list)
	}

	rec = do(t, h, "GET", "/api/tasks/1", "")
	expectStatus(t, rec, http.StatusOK)
	if got := decode[taskResponse](t, rec); got != want {
		t.Fatalf("get = %+v", got)
	}

	rec = do(t, h, "PUT", "/api/tasks/1", `{"assignee":"Carol","date":"2026-11-01","description":"Updated","status":"in_progress"}`)
	expectStatus(t, rec, http.StatusOK)
	updated := decode[taskResponse](t, rec)
	if updated.Assignee != "Carol" || updated.Date != "2026-11-01" || updated.Description != "Updated" || updated.Status != "in_progress" {
		t.Fatalf("updated = %+v", updated)
	}

	rec = do(t, h, "DELETE", "/api/tasks/1", "")
	expectStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body = %q", rec.Body.String())
	}

	rec = do(t, h, "GET", "/api/tasks/1", "")
	expectStatus(t, rec, http.StatusNotFound)
}

func TestNotFound(t *testing.T) {
	h := newTestHandler(newFakeStore())
	body := `{"assignee":"A","date":"2026-10-31","description":"D","status":"done"}`
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/tasks/999", ""},
		{"PUT", "/api/tasks/999", body},
		{"DELETE", "/api/tasks/999", ""},
		{"GET", "/api/tasks/999/comments", ""},
		{"POST", "/api/tasks/999/comments", `{"author":"A","body":"B"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.body)
			expectStatus(t, rec, http.StatusNotFound)
			if got := decode[errorResponse](t, rec); got.Error != "task not found" || got.Fields != nil {
				t.Fatalf("body = %+v", got)
			}
		})
	}
}

func TestInvalidID(t *testing.T) {
	h := newTestHandler(newFakeStore())
	targets := []struct{ method, suffix, body string }{
		{"GET", "", ""},
		{"PUT", "", `{"assignee":"A","date":"2026-10-31","description":"D","status":"done"}`},
		{"DELETE", "", ""},
		{"GET", "/comments", ""},
		{"POST", "/comments", `{"author":"A","body":"B"}`},
	}
	for _, id := range []string{"abc", "0", "-1", "+1", "1.5", "99999999999999999999", "1e3"} {
		for _, tg := range targets {
			t.Run(tg.method+" "+id+tg.suffix, func(t *testing.T) {
				rec := do(t, h, tg.method, "/api/tasks/"+id+tg.suffix, tg.body)
				expectStatus(t, rec, http.StatusBadRequest)
				if got := decode[errorResponse](t, rec); got.Error != "id must be a positive integer" {
					t.Fatalf("body = %+v", got)
				}
			})
		}
	}
}

func TestBadBodies(t *testing.T) {
	h := newTestHandler(newFakeStore())
	tests := []struct {
		name string
		body string
	}{
		{"malformed", `{"assignee":`},
		{"not an object", `[1,2]`},
		{"unknown field", `{"assignee":"A","date":"2026-10-31","description":"D","id":5}`},
		{"wrong type", `{"assignee":7,"date":"2026-10-31","description":"D"}`},
		{"trailing data", `{"assignee":"A","date":"2026-10-31","description":"D"}{}`},
	}
	for _, tc := range tests {
		for _, target := range []struct{ method, path string }{{"POST", "/api/tasks"}, {"PUT", "/api/tasks/1"}} {
			t.Run(target.method+" "+tc.name, func(t *testing.T) {
				rec := do(t, h, target.method, target.path, tc.body)
				expectStatus(t, rec, http.StatusBadRequest)
				if got := decode[errorResponse](t, rec); got.Error == "" || got.Fields != nil {
					t.Fatalf("body = %+v", got)
				}
			})
		}
	}
}

func TestValidationErrorShape(t *testing.T) {
	h := newTestHandler(newFakeStore())
	rec := do(t, h, "POST", "/api/tasks", `{"assignee":"  ","date":"2026-02-30","description":""}`)
	expectStatus(t, rec, http.StatusBadRequest)
	got := decode[errorResponse](t, rec)
	want := map[string]string{
		"assignee":    "is required",
		"date":        "must be a valid date in YYYY-MM-DD format",
		"description": "is required",
	}
	if got.Error != "validation failed" || len(got.Fields) != len(want) {
		t.Fatalf("body = %+v", got)
	}
	for k, v := range want {
		if got.Fields[k] != v {
			t.Fatalf("fields[%s] = %q, want %q", k, got.Fields[k], v)
		}
	}
}

func TestBodyTooLarge(t *testing.T) {
	h := newTestHandler(newFakeStore())
	big := `{"assignee":"A","date":"2026-10-31","description":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	rec := do(t, h, "POST", "/api/tasks", big)
	expectStatus(t, rec, http.StatusRequestEntityTooLarge)
	if got := decode[errorResponse](t, rec); got.Error == "" {
		t.Fatalf("body = %+v", got)
	}
}

func TestRequiresJSONContentType(t *testing.T) {
	h := newTestHandler(newFakeStore())
	req := httptest.NewRequest("POST", "/api/tasks", strings.NewReader(`{"assignee":"A","date":"2026-10-31","description":"D"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusBadRequest)

	req = httptest.NewRequest("POST", "/api/tasks", strings.NewReader(`{"assignee":"A","date":"2026-10-31","description":"D"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusCreated)
}

func TestInternalErrorDoesNotLeak(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("pq: password authentication failed for user secret_user at 10.0.0.1")
	h := newTestHandler(fs)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/tasks", ""},
		{"GET", "/api/tasks/1", ""},
		{"POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D"}`},
		{"PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D","status":"done"}`},
		{"DELETE", "/api/tasks/1", ""},
		{"GET", "/api/tasks/1/comments", ""},
		{"POST", "/api/tasks/1/comments", `{"author":"A","body":"B"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.body)
			expectStatus(t, rec, http.StatusInternalServerError)
			if strings.Contains(rec.Body.String(), "secret_user") {
				t.Fatalf("leaked internals: %s", rec.Body.String())
			}
			if got := decode[errorResponse](t, rec); got.Error != "internal server error" {
				t.Fatalf("body = %+v", got)
			}
		})
	}
}

func TestFallbackRoutesReturnJSON(t *testing.T) {
	h := newTestHandler(newFakeStore())

	rec := do(t, h, "GET", "/nope", "")
	expectStatus(t, rec, http.StatusNotFound)
	decode[errorResponse](t, rec)

	rec = do(t, h, "PATCH", "/api/tasks/1", "")
	expectStatus(t, rec, http.StatusMethodNotAllowed)
	decode[errorResponse](t, rec)

	rec = do(t, h, "DELETE", "/api/tasks", "")
	expectStatus(t, rec, http.StatusMethodNotAllowed)
	decode[errorResponse](t, rec)

	for _, method := range []string{"PUT", "DELETE", "PATCH"} {
		rec = do(t, h, method, "/api/tasks/1/comments", "")
		expectStatus(t, rec, http.StatusMethodNotAllowed)
		if allow := rec.Header().Get("Allow"); allow != "GET, POST, OPTIONS" {
			t.Fatalf("%s comments Allow = %q", method, allow)
		}
		decode[errorResponse](t, rec)
	}

	for _, path := range []string{"/api/tasks/1/comments/2", "/api/tasks/1/other"} {
		rec = do(t, h, "POST", path, `{"message":"hi"}`)
		expectStatus(t, rec, http.StatusNotFound)
		decode[errorResponse](t, rec)
	}
}

func TestTaskStatus(t *testing.T) {
	h := newTestHandler(newFakeStore())

	rec := do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D"}`)
	expectStatus(t, rec, http.StatusCreated)
	if got := decode[taskResponse](t, rec); got.Status != "not_started" {
		t.Fatalf("default status = %q", got.Status)
	}

	rec = do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D","status":"blocked"}`)
	expectStatus(t, rec, http.StatusCreated)
	if got := decode[taskResponse](t, rec); got.Status != "blocked" {
		t.Fatalf("explicit status = %q", got.Status)
	}

	for _, status := range []string{"not_started", "in_progress", "blocked", "done"} {
		rec = do(t, h, "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D","status":"`+status+`"}`)
		expectStatus(t, rec, http.StatusOK)
		if got := decode[taskResponse](t, rec); got.Status != status {
			t.Fatalf("PUT status = %q, want %q", got.Status, status)
		}
	}

	tests := []struct {
		name, method, path, body, msg string
	}{
		{"PUT missing", "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D"}`, "is required"},
		{"PUT null", "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D","status":null}`, "is required"},
		{"PUT invalid", "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D","status":"finished"}`, invalidStatusMsg},
		{"PUT wrong case", "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-31","description":"D","status":"Done"}`, invalidStatusMsg},
		{"POST invalid", "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D","status":"open"}`, invalidStatusMsg},
		{"POST empty", "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D","status":""}`, invalidStatusMsg},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.path, tc.body)
			expectStatus(t, rec, http.StatusBadRequest)
			got := decode[errorResponse](t, rec)
			if got.Error != "validation failed" || len(got.Fields) != 1 || got.Fields["status"] != tc.msg {
				t.Fatalf("body = %+v", got)
			}
		})
	}

	for _, target := range []struct{ method, path string }{{"POST", "/api/tasks"}, {"PUT", "/api/tasks/1"}} {
		rec = do(t, h, target.method, target.path, `{"assignee":"A","date":"2026-10-31","description":"D","status":"done","done":true}`)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields != nil {
			t.Fatalf("done field: body = %+v", got)
		}
	}
}

func TestComments(t *testing.T) {
	h := newTestHandler(newFakeStore())
	do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D"}`)

	rec := do(t, h, "GET", "/api/tasks/1/comments", "")
	expectStatus(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("empty comments body = %q, want []", got)
	}

	rec = do(t, h, "POST", "/api/tasks/1/comments", `{"author":"  Alice ","body":"  line one\nline two  "}`)
	expectStatus(t, rec, http.StatusCreated)
	first := decode[commentResponse](t, rec)
	want := commentResponse{ID: 1, TaskID: 1, Author: "Alice", Body: "line one\nline two", CreatedAt: "2026-10-20T14:00:01Z"}
	if first != want {
		t.Fatalf("created = %+v, want %+v", first, want)
	}

	rec = do(t, h, "POST", "/api/tasks/1/comments", `{"author":"Bob","body":"second"}`)
	expectStatus(t, rec, http.StatusCreated)

	rec = do(t, h, "GET", "/api/tasks/1/comments", "")
	expectStatus(t, rec, http.StatusOK)
	list := decode[[]commentResponse](t, rec)
	if len(list) != 2 || list[0] != first || list[1].ID != 2 || list[1].Author != "Bob" {
		t.Fatalf("list = %+v", list)
	}

	rec = do(t, h, "GET", "/api/tasks/1", "")
	if got := decode[taskResponse](t, rec); got.UpdatedAt != "2026-10-20T13:00:00Z" {
		t.Fatalf("comment changed task updatedAt: %+v", got)
	}
}

func TestCommentValidation(t *testing.T) {
	h := newTestHandler(newFakeStore())
	do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D"}`)

	tests := []struct {
		name   string
		body   string
		fields map[string]string
	}{
		{"both missing", `{}`, map[string]string{"author": "is required", "body": "is required"}},
		{"blank author", `{"author":" \t ","body":"x"}`, map[string]string{"author": "is required"}},
		{"blank body", `{"author":"A","body":"\n  "}`, map[string]string{"body": "is required"}},
		{"long author", `{"author":"` + strings.Repeat("a", 101) + `","body":"x"}`, map[string]string{"author": "must be at most 100 characters"}},
		{"long body", `{"author":"A","body":"` + strings.Repeat("b", 2001) + `"}`, map[string]string{"body": "must be at most 2000 characters"}},
		{"NUL author", `{"author":"a\u0000b","body":"x"}`, map[string]string{"author": "must not contain NUL characters"}},
		{"NUL body", `{"author":"A","body":"a\u0000b"}`, map[string]string{"body": "must not contain NUL characters"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/api/tasks/1/comments", tc.body)
			expectStatus(t, rec, http.StatusBadRequest)
			got := decode[errorResponse](t, rec)
			if got.Error != "validation failed" || len(got.Fields) != len(tc.fields) {
				t.Fatalf("body = %+v", got)
			}
			for k, v := range tc.fields {
				if got.Fields[k] != v {
					t.Fatalf("fields[%s] = %q, want %q", k, got.Fields[k], v)
				}
			}
		})
	}

	t.Run("limits count characters not bytes", func(t *testing.T) {
		body := `{"author":"` + strings.Repeat("é", 100) + `","body":"` + strings.Repeat("日", 2000) + `"}`
		rec := do(t, h, "POST", "/api/tasks/1/comments", body)
		expectStatus(t, rec, http.StatusCreated)
	})

	for _, tc := range []struct{ name, body string }{
		{"malformed", `{"author":`},
		{"not an object", `["a"]`},
		{"unknown field", `{"author":"A","body":"B","taskId":1}`},
		{"wrong type", `{"author":1,"body":"B"}`},
		{"trailing data", `{"author":"A","body":"B"}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/api/tasks/1/comments", tc.body)
			expectStatus(t, rec, http.StatusBadRequest)
			if got := decode[errorResponse](t, rec); got.Error == "" || got.Fields != nil {
				t.Fatalf("body = %+v", got)
			}
		})
	}

	t.Run("body too large", func(t *testing.T) {
		big := `{"author":"A","body":"` + strings.Repeat("x", maxBodyBytes) + `"}`
		rec := do(t, h, "POST", "/api/tasks/1/comments", big)
		expectStatus(t, rec, http.StatusRequestEntityTooLarge)
	})

	t.Run("requires JSON content type", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/tasks/1/comments", strings.NewReader(`{"author":"A","body":"B"}`))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		expectStatus(t, rec, http.StatusBadRequest)
	})
}

func TestCORS(t *testing.T) {
	h := newTestHandler(newFakeStore())

	t.Run("preflight from allowed origin", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/tasks/1", nil)
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Access-Control-Request-Method", "PUT")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		expectStatus(t, rec, http.StatusNoContent)
		hdr := rec.Header()
		if hdr.Get("Access-Control-Allow-Origin") != testOrigin ||
			hdr.Get("Access-Control-Allow-Methods") != "GET, POST, PUT, DELETE, OPTIONS" ||
			hdr.Get("Access-Control-Allow-Headers") != "Content-Type" {
			t.Fatalf("headers = %v", hdr)
		}
	})

	t.Run("simple request from allowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/tasks", nil)
		req.Header.Set("Origin", testOrigin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		expectStatus(t, rec, http.StatusOK)
		if rec.Header().Get("Access-Control-Allow-Origin") != testOrigin {
			t.Fatalf("missing allow-origin")
		}
	})

	t.Run("disallowed origin gets no CORS headers", func(t *testing.T) {
		for _, method := range []string{"OPTIONS", "GET"} {
			req := httptest.NewRequest(method, "/api/tasks", nil)
			req.Header.Set("Origin", "http://evil.example")
			req.Header.Set("Access-Control-Request-Method", "POST")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Methods") != "" {
				t.Fatalf("%s: unexpected CORS headers %v", method, rec.Header())
			}
		}
	})
}
