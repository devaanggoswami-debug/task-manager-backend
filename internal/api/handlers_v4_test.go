package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"taskmanager/backend/internal/store"
)

// fixedNow is a Wednesday.
var fixedNow = time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)

func newClockHandler(s TaskStore) http.Handler {
	return newHandler(s, testOrigin, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return fixedNow })
}

// seed creates legacy sprints S1 (around fixedNow) and S2, which belong to
// no project, and projects Auth and Billing, with ids 1 and 2 each.
func seed(t *testing.T, h http.Handler, fs *fakeStore) {
	t.Helper()
	fs.addSprint(store.SprintInput{Name: "S1", StartDate: day("2026-10-15"), EndDate: day("2026-10-28")})
	fs.addSprint(store.SprintInput{Name: "S2", StartDate: day("2026-10-29"), EndDate: day("2026-11-11")})
	for _, body := range []string{`{"name":"Auth"}`, `{"name":"Billing"}`} {
		expectStatus(t, do(t, h, "POST", "/api/projects", body), http.StatusCreated)
	}
}

func TestTaskV4Fields(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seed(t, h, fs)

	rec := do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"Write docs"}`)
	expectStatus(t, rec, http.StatusCreated)
	if !strings.Contains(rec.Body.String(), `"category":"engineering","sprintIds":[],"projectIds":[]`) {
		t.Fatalf("omitted fields: %s", rec.Body.String())
	}

	rec = do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"Prepare budget","sprintIds":[2,1,2],"projectIds":[2]}`)
	expectStatus(t, rec, http.StatusCreated)
	got := decode[taskView](t, rec)
	if *got.Category != "finance" || !reflect.DeepEqual(got.SprintIDs, []int64{1, 2}) || !reflect.DeepEqual(got.ProjectIDs, []int64{2}) {
		t.Fatalf("classified create: %+v", got)
	}

	rec = do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"Prepare budget","category":"sales"}`)
	expectStatus(t, rec, http.StatusCreated)
	if got := decode[taskView](t, rec); *got.Category != "sales" {
		t.Fatalf("explicit category: %+v", got)
	}

	t.Run("PUT without new fields keeps them", func(t *testing.T) {
		rec := do(t, h, "PUT", "/api/tasks/2", `{"assignee":"B","date":"2026-11-01","description":"Fix bug","status":"in_progress"}`)
		expectStatus(t, rec, http.StatusOK)
		got := decode[taskView](t, rec)
		if *got.Category != "finance" || !reflect.DeepEqual(got.SprintIDs, []int64{1, 2}) || !reflect.DeepEqual(got.ProjectIDs, []int64{2}) || got.Assignee != "B" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("PUT null fields keeps them", func(t *testing.T) {
		rec := do(t, h, "PUT", "/api/tasks/2", `{"assignee":"B","date":"2026-11-01","description":"Fix bug","status":"in_progress","category":null,"sprintIds":null,"projectIds":null}`)
		expectStatus(t, rec, http.StatusOK)
		got := decode[taskView](t, rec)
		if *got.Category != "finance" || len(got.SprintIDs) != 2 || len(got.ProjectIDs) != 1 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("PUT replaces sets and category", func(t *testing.T) {
		rec := do(t, h, "PUT", "/api/tasks/2", `{"assignee":"B","date":"2026-11-01","description":"Fix bug","status":"in_progress","category":"compliance","sprintIds":[2],"projectIds":[]}`)
		expectStatus(t, rec, http.StatusOK)
		got := decode[taskView](t, rec)
		if *got.Category != "compliance" || !reflect.DeepEqual(got.SprintIDs, []int64{2}) || len(got.ProjectIDs) != 0 || got.ProjectIDs == nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("validation", func(t *testing.T) {
		base := `"assignee":"A","date":"2026-10-31","description":"D","status":"done"`
		tests := []struct {
			name, body string
			fields     map[string]string
		}{
			{"bad category", `{` + base + `,"category":"Engineering"}`, map[string]string{"category": invalidCategoryMsg}},
			{"empty category", `{` + base + `,"category":""}`, map[string]string{"category": invalidCategoryMsg}},
			{"zero id", `{` + base + `,"sprintIds":[0]}`, map[string]string{"sprintIds": invalidIDsMsg}},
			{"negative id", `{` + base + `,"projectIds":[3,-1]}`, map[string]string{"projectIds": invalidIDsMsg}},
			{"unknown ids", `{` + base + `,"sprintIds":[9,7,1,9],"projectIds":[5]}`,
				map[string]string{"sprintIds": "unknown id(s): 7, 9", "projectIds": "unknown id(s): 5"}},
		}
		for _, tc := range tests {
			for _, target := range []struct{ method, path string }{{"POST", "/api/tasks"}, {"PUT", "/api/tasks/2"}} {
				t.Run(target.method+" "+tc.name, func(t *testing.T) {
					rec := do(t, h, target.method, target.path, tc.body)
					expectStatus(t, rec, http.StatusBadRequest)
					got := decode[errorResponse](t, rec)
					if got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, tc.fields) {
						t.Fatalf("body = %+v", got)
					}
				})
			}
		}
		for _, body := range []string{`{` + base + `,"sprintIds":[1.5]}`, `{` + base + `,"sprintIds":"1"}`, `{` + base + `,"projectIds":[null]}`} {
			rec := do(t, h, "POST", "/api/tasks", body)
			expectStatus(t, rec, http.StatusBadRequest)
		}
		rec := do(t, h, "GET", "/api/tasks/2", "")
		if got := decode[taskView](t, rec); *got.Category != "compliance" || !reflect.DeepEqual(got.SprintIDs, []int64{2}) {
			t.Fatalf("rejected writes changed task: %+v", got)
		}
		if got := decode[[]taskView](t, do(t, h, "GET", "/api/tasks", "")); len(got) != 3 {
			t.Fatalf("rejected creates stored tasks: %d", len(got))
		}
	})
}

func TestTaskFilters(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seed(t, h, fs)
	for _, body := range []string{
		`{"assignee":"A","date":"2026-10-30","description":"one","sprintIds":[1],"projectIds":[1]}`,
		`{"assignee":"A","date":"2026-10-25","description":"two","sprintIds":[1,2],"projectIds":[1,2]}`,
		`{"assignee":"A","date":"2026-10-20","description":"three","projectIds":[2]}`,
		`{"assignee":"A","date":"2026-10-20","description":"four"}`,
	} {
		expectStatus(t, do(t, h, "POST", "/api/tasks", body), http.StatusCreated)
	}
	ids := func(path string) []int64 {
		rec := do(t, h, "GET", path, "")
		expectStatus(t, rec, http.StatusOK)
		out := []int64{}
		for _, v := range decode[[]taskView](t, rec) {
			out = append(out, v.ID)
		}
		return out
	}
	for path, want := range map[string][]int64{
		"/api/tasks":                        {3, 4, 2, 1},
		"/api/tasks?sprintId=1":             {2, 1},
		"/api/tasks?sprintId=2":             {2},
		"/api/tasks?projectId=2":            {3, 2},
		"/api/tasks?sprintId=1&projectId=2": {2},
		"/api/tasks?sprintId=99":            {},
		"/api/tasks?projectId=99":           {},
	} {
		if got := ids(path); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	for _, q := range []string{"sprintId=abc", "sprintId=0", "sprintId=-1", "sprintId=", "projectId=1.5", "projectId=+2", "sprintId=1&projectId=x"} {
		rec := do(t, h, "GET", "/api/tasks?"+q, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); !strings.HasSuffix(got.Error, "must be a positive integer") {
			t.Fatalf("%s: %+v", q, got)
		}
	}
}

func TestSprints(t *testing.T) {
	h := newClockHandler(newFakeStore())

	rec := do(t, h, "GET", "/api/sprints", "")
	expectStatus(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("empty list = %q", got)
	}
	expectStatus(t, do(t, h, "POST", "/api/projects", `{"name":"Auth"}`), http.StatusCreated)

	rec = do(t, h, "POST", "/api/sprints", `{"name":"  Later ","startDate":"2026-11-01","endDate":"2026-11-01","projectId":1}`)
	expectStatus(t, rec, http.StatusCreated)
	project := int64(1)
	want := sprintResponse{ID: 1, Name: "Later", StartDate: "2026-11-01", EndDate: "2026-11-01", ProjectID: &project, CreatedAt: "2026-10-20T12:00:00Z"}
	if got := decode[sprintResponse](t, rec); !reflect.DeepEqual(got, want) {
		t.Fatalf("created = %+v", got)
	}
	expectStatus(t, do(t, h, "POST", "/api/sprints", `{"name":"Earlier","startDate":"2026-10-01","endDate":"2026-10-14","projectId":1}`), http.StatusCreated)

	rec = do(t, h, "POST", "/api/sprints", `{"name":"later","startDate":"2026-12-01","endDate":"2026-12-02","projectId":1}`)
	expectStatus(t, rec, http.StatusConflict)
	if got := decode[errorResponse](t, rec); got.Error != "a sprint with that name already exists" || got.Fields != nil {
		t.Fatalf("409 body = %+v", got)
	}

	list := decode[[]sprintResponse](t, do(t, h, "GET", "/api/sprints", ""))
	if len(list) != 2 || list[0].Name != "Earlier" || list[1].Name != "Later" {
		t.Fatalf("list = %+v", list)
	}

	expectStatus(t, do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-11-01","description":"D","sprintIds":[1]}`), http.StatusCreated)
	rec = do(t, h, "GET", "/api/sprints/1", "")
	expectStatus(t, rec, http.StatusOK)
	if got := decode[sprintResponse](t, rec); got.TaskCount != 1 || got.Name != "Later" {
		t.Fatalf("get = %+v", got)
	}

	rec = do(t, h, "GET", "/api/sprints/1/status", "")
	expectStatus(t, rec, http.StatusOK)
	if got := decode[statusResponse](t, rec); got != (statusResponse{Status: "on_track", Reason: "0 of 1 tasks done, 1 day left", Total: 1}) {
		t.Fatalf("status = %+v", got)
	}
	rec = do(t, h, "GET", "/api/sprints/2/status", "")
	if got := decode[statusResponse](t, rec); got != (statusResponse{Status: "on_track", Reason: "no tasks yet"}) {
		t.Fatalf("empty status = %+v", got)
	}

	for _, path := range []string{"/api/sprints/9", "/api/sprints/9/status"} {
		rec = do(t, h, "GET", path, "")
		expectStatus(t, rec, http.StatusNotFound)
		if got := decode[errorResponse](t, rec); got.Error != "sprint not found" {
			t.Fatalf("%s: %+v", path, got)
		}
	}
	for _, path := range []string{"/api/sprints/abc", "/api/sprints/0/status"} {
		expectStatus(t, do(t, h, "GET", path, ""), http.StatusBadRequest)
	}

	tests := []struct {
		name, body string
		fields     map[string]string
	}{
		{"all missing", `{}`, map[string]string{"name": "is required", "startDate": "is required", "endDate": "is required", "projectId": "is required"}},
		{"blank name", `{"name":"  ","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`, map[string]string{"name": "is required"}},
		{"long name", `{"name":"` + strings.Repeat("n", 101) + `","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`, map[string]string{"name": "must be at most 100 characters"}},
		{"bad dates", `{"name":"X","startDate":"2026-02-30","endDate":"tomorrow","projectId":1}`, map[string]string{"startDate": invalidDateMsg, "endDate": invalidDateMsg}},
		{"end before start", `{"name":"X","startDate":"2026-10-02","endDate":"2026-10-01","projectId":1}`, map[string]string{"endDate": "must be on or after startDate"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/api/sprints", tc.body)
			expectStatus(t, rec, http.StatusBadRequest)
			if got := decode[errorResponse](t, rec); got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, tc.fields) {
				t.Fatalf("body = %+v", got)
			}
		})
	}
	for _, body := range []string{`{"name":"X","startDate":"2026-10-01","endDate":"2026-10-02","id":1}`, `[]`, `{"name":1}`} {
		rec := do(t, h, "POST", "/api/sprints", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields != nil {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	big := `{"name":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	expectStatus(t, do(t, h, "POST", "/api/sprints", big), http.StatusRequestEntityTooLarge)
}

func TestProjects(t *testing.T) {
	h := newClockHandler(newFakeStore())

	rec := do(t, h, "POST", "/api/projects", `{"name":" billing "}`)
	expectStatus(t, rec, http.StatusCreated)
	want := projectResponse{ID: 1, Name: "billing", CreatedAt: "2026-10-20T12:00:00Z"}
	if got := decode[projectResponse](t, rec); got != want {
		t.Fatalf("created = %+v", got)
	}
	expectStatus(t, do(t, h, "POST", "/api/projects", `{"name":"Auth"}`), http.StatusCreated)

	rec = do(t, h, "POST", "/api/projects", `{"name":"BILLING"}`)
	expectStatus(t, rec, http.StatusConflict)
	if got := decode[errorResponse](t, rec); got.Error != "a project with that name already exists" {
		t.Fatalf("409 body = %+v", got)
	}

	list := decode[[]projectResponse](t, do(t, h, "GET", "/api/projects", ""))
	if len(list) != 2 || list[0].Name != "Auth" || list[1].Name != "billing" {
		t.Fatalf("list = %+v", list)
	}

	expectStatus(t, do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-01","description":"D","projectIds":[1]}`), http.StatusCreated)
	if got := decode[projectResponse](t, do(t, h, "GET", "/api/projects/1", "")); got.TaskCount != 1 {
		t.Fatalf("get = %+v", got)
	}
	rec = do(t, h, "GET", "/api/projects/1/status", "")
	expectStatus(t, rec, http.StatusOK)
	if got := decode[statusResponse](t, rec); got != (statusResponse{Status: "at_risk", Reason: `1 overdue task: #1 "D"`, Total: 1, Overdue: 1}) {
		t.Fatalf("status = %+v", got)
	}

	for _, path := range []string{"/api/projects/9", "/api/projects/9/status"} {
		rec = do(t, h, "GET", path, "")
		expectStatus(t, rec, http.StatusNotFound)
		if got := decode[errorResponse](t, rec); got.Error != "project not found" {
			t.Fatalf("%s: %+v", path, got)
		}
	}
	expectStatus(t, do(t, h, "GET", "/api/projects/x/status", ""), http.StatusBadRequest)

	for body, fields := range map[string]map[string]string{
		`{}`: {"name": "is required"},
		`{"name":"` + strings.Repeat("é", 101) + `"}`: {"name": "must be at most 100 characters"},
	} {
		rec := do(t, h, "POST", "/api/projects", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, fields) {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	expectStatus(t, do(t, h, "POST", "/api/projects", `{"name":"X","taskCount":1}`), http.StatusBadRequest)
}

func TestV4FallbackRoutes(t *testing.T) {
	h := newClockHandler(newFakeStore())
	for _, tc := range []struct{ method, path, allow string }{
		{"DELETE", "/api/sprints", "GET, POST, OPTIONS"},
		{"PUT", "/api/projects", "GET, POST, OPTIONS"},
		{"PATCH", "/api/sprints/1", "GET, PUT, OPTIONS"},
		{"DELETE", "/api/projects/1", "GET, PUT, OPTIONS"},
		{"POST", "/api/sprints/1/status", "GET, OPTIONS"},
		{"PATCH", "/api/projects/1/status", "GET, OPTIONS"},
		{"GET", "/api/chat", "POST, OPTIONS"},
		{"PUT", "/api/chat", "POST, OPTIONS"},
	} {
		rec := do(t, h, tc.method, tc.path, "")
		expectStatus(t, rec, http.StatusMethodNotAllowed)
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s Allow = %q", tc.method, tc.path, allow)
		}
		decode[errorResponse](t, rec)
	}
	for _, path := range []string{"/api/sprints/1/other", "/api/projects/1/status/x", "/api/chat/1", "/api/sprint"} {
		rec := do(t, h, "GET", path, "")
		expectStatus(t, rec, http.StatusNotFound)
		decode[errorResponse](t, rec)
	}
}

func TestV4InternalErrorDoesNotLeak(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("pq: secret_user")
	h := newClockHandler(fs)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/tasks?sprintId=1", ""},
		{"GET", "/api/sprints", ""},
		{"POST", "/api/sprints", `{"name":"S","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`},
		{"GET", "/api/sprints/1", ""},
		{"GET", "/api/sprints/1/status", ""},
		{"GET", "/api/projects", ""},
		{"POST", "/api/projects", `{"name":"P"}`},
		{"GET", "/api/projects/1", ""},
		{"GET", "/api/projects/1/status", ""},
		{"POST", "/api/chat", `{"message":"status of sprint S1"}`},
		{"POST", "/api/chat", `{"message":"create task \"x\" for A due today"}`},
	} {
		rec := do(t, h, tc.method, tc.path, tc.body)
		expectStatus(t, rec, http.StatusInternalServerError)
		if strings.Contains(rec.Body.String(), "secret_user") {
			t.Fatalf("leaked: %s", rec.Body.String())
		}
	}
}

func TestToViewNeverNullLists(t *testing.T) {
	v := toView(store.Task{})
	if v.SprintIDs == nil || v.ProjectIDs == nil || v.Category != nil {
		t.Fatalf("view = %+v", v)
	}
}
