package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"taskmanager/backend/internal/store"
)

// seedV5 creates projects Auth (1) and Billing (2), sprints "Sprint 1" in
// Auth (1), "Sprint 1" in Billing (2) and a legacy "Old" sprint with no
// project (3).
func seedV5(t *testing.T, h http.Handler, fs *fakeStore) {
	t.Helper()
	for _, body := range []string{`{"name":"Auth"}`, `{"name":"Billing"}`} {
		expectStatus(t, do(t, h, "POST", "/api/projects", body), http.StatusCreated)
	}
	for _, body := range []string{
		`{"name":"Sprint 1","startDate":"2026-10-15","endDate":"2026-10-28","projectId":1}`,
		`{"name":"Sprint 1","startDate":"2026-10-15","endDate":"2026-10-28","projectId":2}`,
	} {
		expectStatus(t, do(t, h, "POST", "/api/sprints", body), http.StatusCreated)
	}
	fs.addSprint(store.SprintInput{Name: "Old", StartDate: day("2026-10-01"), EndDate: day("2026-10-14")})
}

func taskIDs(t *testing.T, rec *httptest.ResponseRecorder) []int64 {
	t.Helper()
	out := []int64{}
	for _, v := range decode[[]taskView](t, rec) {
		out = append(out, v.ID)
	}
	return out
}

func totalCount(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	n, err := strconv.Atoi(rec.Header().Get(totalCountHeader))
	if err != nil {
		t.Fatalf("%s = %q", totalCountHeader, rec.Header().Get(totalCountHeader))
	}
	return n
}

func TestTaskPagination(t *testing.T) {
	h := newClockHandler(newFakeStore())
	for i := 1; i <= 25; i++ {
		body := fmt.Sprintf(`{"assignee":"A","date":"2026-11-%02d","description":"task %d"}`, i+1, i)
		expectStatus(t, do(t, h, "POST", "/api/tasks", body), http.StatusCreated)
	}

	for _, tc := range []struct {
		query    string
		from, to int64
	}{
		{"", 1, 25},
		{"?page=1", 1, 20},
		{"?page=2", 21, 25},
		{"?pageSize=10", 1, 10},
		{"?page=3&pageSize=10", 21, 25},
		{"?page=2&pageSize=100", 0, -1},
		{"?page=4&pageSize=10", 0, -1},
		{"?page=9223372036854775807&pageSize=100", 0, -1},
		{"?pageSize=1&page=25", 25, 25},
	} {
		rec := do(t, h, "GET", "/api/tasks"+tc.query, "")
		expectStatus(t, rec, http.StatusOK)
		want := []int64{}
		for id := tc.from; id <= tc.to && tc.from > 0; id++ {
			want = append(want, id)
		}
		if got := taskIDs(t, rec); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ids = %v, want %v", tc.query, got, want)
		}
		if got := totalCount(t, rec); got != 25 {
			t.Errorf("%s: total = %d", tc.query, got)
		}
	}

	for _, tc := range []struct {
		query  string
		fields map[string]string
	}{
		{"page=0", map[string]string{"page": invalidPageMsg}},
		{"page=-1", map[string]string{"page": invalidPageMsg}},
		{"page=abc", map[string]string{"page": invalidPageMsg}},
		{"page=", map[string]string{"page": invalidPageMsg}},
		{"page=1.5", map[string]string{"page": invalidPageMsg}},
		{"page=99999999999999999999", map[string]string{"page": invalidPageMsg}},
		{"pageSize=0", map[string]string{"pageSize": invalidPageSizeMsg}},
		{"pageSize=101", map[string]string{"pageSize": invalidPageSizeMsg}},
		{"pageSize=x&page=0", map[string]string{"page": invalidPageMsg, "pageSize": invalidPageSizeMsg}},
	} {
		rec := do(t, h, "GET", "/api/tasks?"+tc.query, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, tc.fields) {
			t.Errorf("%s: %+v", tc.query, got)
		}
		if rec.Header().Get(totalCountHeader) != "" {
			t.Errorf("%s: total header on error", tc.query)
		}
	}

	rec := do(t, h, "GET", "/api/tasks?pageSize=100", "")
	if len(taskIDs(t, rec)) != 25 {
		t.Fatalf("pageSize=100 must be accepted")
	}
}

func TestTaskSearchFilters(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)
	for _, body := range []string{
		`{"assignee":"Alice","date":"2026-10-20","description":"Give 50% off","status":"done","category":"sales","sprintIds":[1]}`,
		`{"assignee":"Bob","date":"2026-10-21","description":"Give 500 off","status":"blocked","category":"sales"}`,
		`{"assignee":"Carol_X","date":"2026-10-22","description":"Fix login bug","category":"engineering","sprintIds":[1]}`,
		`{"assignee":"CarolYX","date":"2026-10-23","description":"Fix logout bug","category":"engineering"}`,
	} {
		expectStatus(t, do(t, h, "POST", "/api/tasks", body), http.StatusCreated)
	}
	fs.mu.Lock()
	legacy := fs.tasks[4]
	legacy.Category = nil
	fs.tasks[4] = legacy
	fs.mu.Unlock()

	for query, want := range map[string][]int64{
		"status=done":                      {1},
		"status=blocked":                   {2},
		"status=not_started":               {3, 4},
		"category=sales":                   {1, 2},
		"category=unclassified":            {4},
		"q=50%25":                          {1},
		"q=carol_":                         {3},
		"q=LOG":                            {3, 4},
		"q=alice":                          {1},
		"q=%20%20bob%20":                   {2},
		"q=%20":                            {1, 2, 3, 4},
		"q=nomatch":                        {},
		"excludeSprintId=1":                {2, 4},
		"excludeSprintId=99":               {1, 2, 3, 4},
		"excludeSprintId=1&q=fix":          {4},
		"sprintId=1&status=done":           {1},
		"projectId=1&category=sales":       {1},
		"category=sales&page=2&pageSize=1": {2},
	} {
		rec := do(t, h, "GET", "/api/tasks?"+query, "")
		expectStatus(t, rec, http.StatusOK)
		if got := taskIDs(t, rec); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
	rec := do(t, h, "GET", "/api/tasks?category=sales&page=2&pageSize=1", "")
	if got := totalCount(t, rec); got != 2 {
		t.Fatalf("filtered total = %d, want 2", got)
	}

	for query, fields := range map[string]map[string]string{
		"status=open":                   {"status": invalidStatusMsg},
		"status=":                       {"status": invalidStatusMsg},
		"category=Sales":                {"category": invalidFilterCategoryMsg},
		"category=":                     {"category": invalidFilterCategoryMsg},
		"q=" + strings.Repeat("x", 101): {"q": "must be at most 100 characters"},
		"q=a%00b":                       {"q": "must not contain NUL characters"},
		"status=x&category=y&page=0":    {"status": invalidStatusMsg, "category": invalidFilterCategoryMsg, "page": invalidPageMsg},
	} {
		rec := do(t, h, "GET", "/api/tasks?"+query, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, fields) {
			t.Errorf("%s: %+v", query, got)
		}
	}
	if !strings.Contains(invalidFilterCategoryMsg, "compliance, unclassified") {
		t.Fatalf("category message = %q", invalidFilterCategoryMsg)
	}
	expectStatus(t, do(t, h, "GET", "/api/tasks?q="+strings.Repeat("é", 100), ""), http.StatusOK)
	for _, q := range []string{"excludeSprintId=0", "excludeSprintId=x", "excludeSprintId="} {
		rec := do(t, h, "GET", "/api/tasks?"+q, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Error != "excludeSprintId must be a positive integer" {
			t.Fatalf("%s: %+v", q, got)
		}
	}
}

func TestSprintsV5(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)

	rec := do(t, h, "GET", "/api/sprints/1", "")
	expectStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"projectId":1,`) {
		t.Fatalf("sprint JSON: %s", rec.Body.String())
	}
	if rec := do(t, h, "GET", "/api/sprints/3", ""); !strings.Contains(rec.Body.String(), `"projectId":null,`) {
		t.Fatalf("legacy sprint JSON: %s", rec.Body.String())
	}

	t.Run("create validation", func(t *testing.T) {
		base := `"name":"S","startDate":"2026-10-01","endDate":"2026-10-02"`
		for body, fields := range map[string]map[string]string{
			`{` + base + `}`:                  {"projectId": "is required"},
			`{` + base + `,"projectId":null}`: {"projectId": "is required"},
			`{` + base + `,"projectId":0}`:    {"projectId": unknownProjectMsg},
			`{` + base + `,"projectId":-3}`:   {"projectId": unknownProjectMsg},
			`{` + base + `,"projectId":99}`:   {"projectId": unknownProjectMsg},
		} {
			for _, target := range []struct{ method, path string }{{"POST", "/api/sprints"}, {"PUT", "/api/sprints/1"}} {
				rec := do(t, h, target.method, target.path, body)
				expectStatus(t, rec, http.StatusBadRequest)
				if got := decode[errorResponse](t, rec); got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, fields) {
					t.Fatalf("%s %s: %+v", target.method, body, got)
				}
			}
		}
		for _, body := range []string{`{` + base + `,"projectId":"1"}`, `{` + base + `,"projectId":1.5}`, `{` + base + `,"projectId":1,"x":1}`} {
			rec := do(t, h, "POST", "/api/sprints", body)
			expectStatus(t, rec, http.StatusBadRequest)
			if got := decode[errorResponse](t, rec); got.Fields != nil {
				t.Fatalf("%s: %+v", body, got)
			}
		}
	})

	t.Run("name unique per project", func(t *testing.T) {
		rec := do(t, h, "POST", "/api/sprints", `{"name":"SPRINT 1","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`)
		expectStatus(t, rec, http.StatusConflict)
		if got := decode[errorResponse](t, rec); got.Error != "a sprint with that name already exists" {
			t.Fatalf("409 body = %+v", got)
		}
		rec = do(t, h, "PUT", "/api/sprints/2", `{"name":"sprint 1","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`)
		expectStatus(t, rec, http.StatusConflict)
		rec = do(t, h, "PUT", "/api/sprints/1", `{"name":"sprint 1","startDate":"2026-10-15","endDate":"2026-10-28","projectId":1}`)
		expectStatus(t, rec, http.StatusOK)
		if got := decode[sprintResponse](t, rec); got.Name != "sprint 1" {
			t.Fatalf("renaming to own name in other case: %+v", got)
		}
		expectStatus(t, do(t, h, "PUT", "/api/sprints/1", `{"name":"Sprint 1","startDate":"2026-10-15","endDate":"2026-10-28","projectId":1}`), http.StatusOK)
	})

	t.Run("update", func(t *testing.T) {
		expectStatus(t, do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-20","description":"in old","sprintIds":[3]}`), http.StatusCreated)
		expectStatus(t, do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-21","description":"in old too","sprintIds":[3],"projectIds":[1]}`), http.StatusCreated)
		if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/1", "")); len(got.ProjectIDs) != 0 {
			t.Fatalf("legacy sprint added a project: %+v", got)
		}

		rec := do(t, h, "PUT", "/api/sprints/3", `{"name":" Old2 ","startDate":"2026-10-02","endDate":"2026-10-15","projectId":2}`)
		expectStatus(t, rec, http.StatusOK)
		project := int64(2)
		want := sprintResponse{ID: 3, Name: "Old2", StartDate: "2026-10-02", EndDate: "2026-10-15", ProjectID: &project, TaskCount: 2,
			CreatedAt: "2026-10-20T12:00:00Z"}
		if got := decode[sprintResponse](t, rec); !reflect.DeepEqual(got, want) {
			t.Fatalf("updated = %+v", got)
		}
		for id, projects := range map[string][]int64{"1": {2}, "2": {1, 2}} {
			if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/"+id, "")); !reflect.DeepEqual(got.ProjectIDs, projects) {
				t.Fatalf("task %s projects = %v, want %v", id, got.ProjectIDs, projects)
			}
		}

		rec = do(t, h, "PUT", "/api/sprints/3", `{"name":"Old2","startDate":"2026-10-02","endDate":"2026-10-15","projectId":1}`)
		expectStatus(t, rec, http.StatusOK)
		if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/1", "")); !reflect.DeepEqual(got.ProjectIDs, []int64{1, 2}) {
			t.Fatalf("changing project must add, not replace: %+v", got.ProjectIDs)
		}
		if got := decode[projectResponse](t, do(t, h, "GET", "/api/projects/1", "")); got.SprintCount != 2 {
			t.Fatalf("sprintCount = %+v", got)
		}

		rec = do(t, h, "PUT", "/api/sprints/99", `{"name":"X","startDate":"2026-10-02","endDate":"2026-10-15","projectId":1}`)
		expectStatus(t, rec, http.StatusNotFound)
		if got := decode[errorResponse](t, rec); got.Error != "sprint not found" {
			t.Fatalf("404 body = %+v", got)
		}
		rec = do(t, h, "PUT", "/api/sprints/1", `{"name":"","startDate":"x","endDate":"2026-10-15","projectId":1}`)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, map[string]string{"name": "is required", "startDate": invalidDateMsg}) {
			t.Fatalf("validation = %+v", got)
		}
		expectStatus(t, do(t, h, "PUT", "/api/sprints/abc", `{}`), http.StatusBadRequest)
		expectStatus(t, do(t, h, "PUT", "/api/sprints/1", `{"name":"`+strings.Repeat("x", maxBodyBytes)+`"}`), http.StatusRequestEntityTooLarge)
	})
}

func TestSprintListFilters(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)
	expectStatus(t, do(t, h, "POST", "/api/sprints", `{"name":"Sprint 0","startDate":"2026-09-01","endDate":"2026-09-14","projectId":1}`), http.StatusCreated)

	ids := func(query string) ([]int64, int) {
		rec := do(t, h, "GET", "/api/sprints"+query, "")
		expectStatus(t, rec, http.StatusOK)
		out := []int64{}
		for _, s := range decode[[]sprintResponse](t, rec) {
			out = append(out, s.ID)
		}
		return out, totalCount(t, rec)
	}
	for query, want := range map[string][]int64{
		"":                               {4, 3, 1, 2},
		"?projectId=1":                   {4, 1},
		"?projectId=2":                   {2},
		"?projectId=99":                  {},
		"?unassigned=true":               {3},
		"?unassigned=false":              {4, 3, 1, 2},
		"?projectId=1&unassigned=true":   {},
		"?projectId=1&page=2&pageSize=1": {1},
		"?page=3&pageSize=2":             {},
	} {
		got, total := ids(query)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
		if query == "?projectId=1&page=2&pageSize=1" && total != 2 {
			t.Errorf("%s total = %d", query, total)
		}
		if query == "?page=3&pageSize=2" && total != 4 {
			t.Errorf("%s total = %d", query, total)
		}
	}
	for query, fields := range map[string]map[string]string{
		"unassigned=yes":          {"unassigned": invalidBoolMsg},
		"unassigned=":             {"unassigned": invalidBoolMsg},
		"unassigned=1&pageSize=0": {"unassigned": invalidBoolMsg, "pageSize": invalidPageSizeMsg},
	} {
		rec := do(t, h, "GET", "/api/sprints?"+query, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, fields) {
			t.Errorf("%s: %+v", query, got)
		}
	}
	rec := do(t, h, "GET", "/api/sprints?projectId=0", "")
	expectStatus(t, rec, http.StatusBadRequest)
	if got := decode[errorResponse](t, rec); got.Error != "projectId must be a positive integer" {
		t.Fatalf("got %+v", got)
	}
}

func TestProjectsV5(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)
	expectStatus(t, do(t, h, "POST", "/api/projects", `{"name":"100% Done_x"}`), http.StatusCreated)

	rec := do(t, h, "GET", "/api/projects", "")
	expectStatus(t, rec, http.StatusOK)
	if got := totalCount(t, rec); got != 3 {
		t.Fatalf("total = %d", got)
	}
	list := decode[[]projectResponse](t, rec)
	if len(list) != 3 || list[0].Name != "100% Done_x" || list[1].Name != "Auth" || list[1].SprintCount != 1 || list[0].SprintCount != 0 {
		t.Fatalf("list = %+v", list)
	}

	for query, want := range map[string][]string{
		"q=AUTH":            {"Auth"},
		"q=%25":             {"100% Done_x"},
		"q=e_":              {"100% Done_x"},
		"q=i":               {"Billing"},
		"q=l":               {"Billing"},
		"page=2&pageSize=2": {"Billing"},
		"page=5":            {},
	} {
		rec := do(t, h, "GET", "/api/projects?"+query, "")
		expectStatus(t, rec, http.StatusOK)
		got := []string{}
		for _, p := range decode[[]projectResponse](t, rec) {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
	rec = do(t, h, "GET", "/api/projects?q="+strings.Repeat("q", 101), "")
	expectStatus(t, rec, http.StatusBadRequest)
	if got := decode[errorResponse](t, rec); got.Fields["q"] != "must be at most 100 characters" {
		t.Fatalf("got %+v", got)
	}

	rec = do(t, h, "PUT", "/api/projects/1", `{"name":"  Identity "}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[projectResponse](t, rec); got != (projectResponse{ID: 1, Name: "Identity", SprintCount: 1, CreatedAt: "2026-10-20T12:00:00Z"}) {
		t.Fatalf("renamed = %+v", got)
	}
	expectStatus(t, do(t, h, "PUT", "/api/projects/1", `{"name":"identity"}`), http.StatusOK)
	rec = do(t, h, "PUT", "/api/projects/1", `{"name":"BILLING"}`)
	expectStatus(t, rec, http.StatusConflict)
	if got := decode[errorResponse](t, rec); got.Error != "a project with that name already exists" {
		t.Fatalf("409 = %+v", got)
	}
	rec = do(t, h, "PUT", "/api/projects/9", `{"name":"X"}`)
	expectStatus(t, rec, http.StatusNotFound)
	if got := decode[errorResponse](t, rec); got.Error != "project not found" {
		t.Fatalf("404 = %+v", got)
	}
	rec = do(t, h, "PUT", "/api/projects/1", `{"name":" "}`)
	expectStatus(t, rec, http.StatusBadRequest)
	if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, map[string]string{"name": "is required"}) {
		t.Fatalf("validation = %+v", got)
	}
	for _, body := range []string{`{"name":"X","id":1}`, `[]`} {
		rec := do(t, h, "PUT", "/api/projects/1", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields != nil {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	expectStatus(t, do(t, h, "PUT", "/api/projects/0", `{"name":"X"}`), http.StatusBadRequest)
	if got := decode[projectResponse](t, do(t, h, "GET", "/api/projects/2", "")); got.SprintCount != 1 {
		t.Fatalf("get sprintCount = %+v", got)
	}
}

func TestSprintTaskMembership(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)
	for _, body := range []string{
		`{"assignee":"A","date":"2026-10-20","description":"one"}`,
		`{"assignee":"A","date":"2026-10-21","description":"two","projectIds":[2]}`,
	} {
		expectStatus(t, do(t, h, "POST", "/api/tasks", body), http.StatusCreated)
	}

	rec := do(t, h, "POST", "/api/sprints/1/tasks", `{"taskIds":[2,1,2]}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[sprintResponse](t, rec); got.ID != 1 || got.TaskCount != 2 {
		t.Fatalf("sprint = %+v", got)
	}
	for id, projects := range map[string][]int64{"1": {1}, "2": {1, 2}} {
		got := decode[taskView](t, do(t, h, "GET", "/api/tasks/"+id, ""))
		if !reflect.DeepEqual(got.SprintIDs, []int64{1}) || !reflect.DeepEqual(got.ProjectIDs, projects) {
			t.Fatalf("task %s = %+v", id, got)
		}
	}
	rec = do(t, h, "POST", "/api/sprints/1/tasks", `{"taskIds":[1]}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[sprintResponse](t, rec); got.TaskCount != 2 {
		t.Fatalf("re-adding is not idempotent: %+v", got)
	}
	expectStatus(t, do(t, h, "POST", "/api/sprints/3/tasks", `{"taskIds":[1]}`), http.StatusOK)
	if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/1", "")); !reflect.DeepEqual(got.SprintIDs, []int64{1, 3}) || !reflect.DeepEqual(got.ProjectIDs, []int64{1}) {
		t.Fatalf("legacy sprint tag: %+v", got)
	}

	rec = do(t, h, "POST", "/api/sprints/2/tasks", `{"taskIds":[1,9,7,9]}`)
	expectStatus(t, rec, http.StatusBadRequest)
	if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, map[string]string{"taskIds": "unknown id(s): 7, 9"}) {
		t.Fatalf("unknown = %+v", got)
	}
	if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/1", "")); slices.Contains(got.SprintIDs, 2) {
		t.Fatalf("rejected add changed task: %+v", got)
	}
	rec = do(t, h, "POST", "/api/sprints/99/tasks", `{"taskIds":[1]}`)
	expectStatus(t, rec, http.StatusNotFound)
	if got := decode[errorResponse](t, rec); got.Error != "sprint not found" {
		t.Fatalf("404 = %+v", got)
	}

	many := make([]string, 101)
	for i := range many {
		many[i] = strconv.Itoa(i + 1)
	}
	for body, msg := range map[string]string{
		`{}`:                 "is required",
		`{"taskIds":null}`:   "is required",
		`{"taskIds":[]}`:     "is required",
		`{"taskIds":[0]}`:    invalidIDsMsg,
		`{"taskIds":[1,-2]}`: invalidIDsMsg,
		`{"taskIds":[` + strings.Join(many, ",") + `]}`: "must contain at most 100 ids",
	} {
		rec := do(t, h, "POST", "/api/sprints/1/tasks", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Error != "validation failed" || !reflect.DeepEqual(got.Fields, map[string]string{"taskIds": msg}) {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	for _, body := range []string{`{"taskIds":[1],"x":1}`, `{"taskIds":["1"]}`, `[1]`, `{"taskIds":[1]}{}`} {
		rec := do(t, h, "POST", "/api/sprints/1/tasks", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields != nil {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	expectStatus(t, do(t, h, "POST", "/api/sprints/x/tasks", `{"taskIds":[1]}`), http.StatusBadRequest)

	rec = do(t, h, "DELETE", "/api/sprints/1/tasks/2", "")
	expectStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body = %q", rec.Body.String())
	}
	if got := decode[taskView](t, do(t, h, "GET", "/api/tasks/2", "")); len(got.SprintIDs) != 0 || !reflect.DeepEqual(got.ProjectIDs, []int64{1, 2}) {
		t.Fatalf("untag must keep projects: %+v", got)
	}
	for path, msg := range map[string]string{
		"/api/sprints/1/tasks/2":  "task is not in that sprint",
		"/api/sprints/99/tasks/1": "sprint not found",
		"/api/sprints/1/tasks/99": "task not found",
	} {
		rec := do(t, h, "DELETE", path, "")
		expectStatus(t, rec, http.StatusNotFound)
		if got := decode[errorResponse](t, rec); got.Error != msg {
			t.Fatalf("%s: %+v", path, got)
		}
	}
	for path, msg := range map[string]string{
		"/api/sprints/0/tasks/1":   "id must be a positive integer",
		"/api/sprints/1/tasks/abc": "taskId must be a positive integer",
	} {
		rec := do(t, h, "DELETE", path, "")
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Error != msg {
			t.Fatalf("%s: %+v", path, got)
		}
	}
}

func TestTaskWritesAddSprintProjects(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)

	rec := do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-20","description":"D","sprintIds":[2,3]}`)
	expectStatus(t, rec, http.StatusCreated)
	if got := decode[taskView](t, rec); !reflect.DeepEqual(got.ProjectIDs, []int64{2}) {
		t.Fatalf("create = %+v", got)
	}
	put := `{"assignee":"A","date":"2026-10-20","description":"D","status":"done","sprintIds":[1],"projectIds":[]}`
	rec = do(t, h, "PUT", "/api/tasks/1", put)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[taskView](t, rec); !reflect.DeepEqual(got.SprintIDs, []int64{1}) || !reflect.DeepEqual(got.ProjectIDs, []int64{1}) {
		t.Fatalf("PUT sprintIds = %+v", got)
	}
	rec = do(t, h, "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-20","description":"D","status":"done","projectIds":[2]}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[taskView](t, rec); !reflect.DeepEqual(got.ProjectIDs, []int64{1, 2}) {
		t.Fatalf("projectIds-only PUT must keep the sprint's project: %+v", got)
	}
	rec = do(t, h, "PUT", "/api/tasks/1", `{"assignee":"A","date":"2026-10-20","description":"D","status":"done","sprintIds":[],"projectIds":[1]}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[taskView](t, rec); len(got.SprintIDs) != 0 || !reflect.DeepEqual(got.ProjectIDs, []int64{1}) {
		t.Fatalf("clearing sprints must keep projects: %+v", got)
	}
}

func TestCommentPagination(t *testing.T) {
	h := newClockHandler(newFakeStore())
	expectStatus(t, do(t, h, "POST", "/api/tasks", `{"assignee":"A","date":"2026-10-31","description":"D"}`), http.StatusCreated)

	rec := do(t, h, "GET", "/api/tasks/1/comments", "")
	expectStatus(t, rec, http.StatusOK)
	if got := totalCount(t, rec); got != 0 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty = %d %s", got, rec.Body.String())
	}
	for i := 1; i <= 5; i++ {
		expectStatus(t, do(t, h, "POST", "/api/tasks/1/comments", `{"author":"A","body":"c`+strconv.Itoa(i)+`"}`), http.StatusCreated)
	}
	for query, want := range map[string][]string{
		"":                   {"c1", "c2", "c3", "c4", "c5"},
		"?pageSize=2":        {"c1", "c2"},
		"?page=3&pageSize=2": {"c5"},
		"?page=2":            {},
	} {
		rec := do(t, h, "GET", "/api/tasks/1/comments"+query, "")
		expectStatus(t, rec, http.StatusOK)
		got := []string{}
		for _, c := range decode[[]commentResponse](t, rec) {
			got = append(got, c.Body)
		}
		if !reflect.DeepEqual(got, want) || totalCount(t, rec) != 5 {
			t.Errorf("%s = %v (total %s), want %v", query, got, rec.Header().Get(totalCountHeader), want)
		}
	}
	rec = do(t, h, "GET", "/api/tasks/1/comments?pageSize=500", "")
	expectStatus(t, rec, http.StatusBadRequest)
	if got := decode[errorResponse](t, rec); !reflect.DeepEqual(got.Fields, map[string]string{"pageSize": invalidPageSizeMsg}) {
		t.Fatalf("got %+v", got)
	}
	rec = do(t, h, "GET", "/api/tasks/9/comments?page=1", "")
	expectStatus(t, rec, http.StatusNotFound)
}

func TestV5FallbackRoutes(t *testing.T) {
	h := newClockHandler(newFakeStore())
	for _, tc := range []struct{ method, path, allow string }{
		{"PATCH", "/api/sprints/1", "GET, PUT, OPTIONS"},
		{"DELETE", "/api/sprints/1", "GET, PUT, OPTIONS"},
		{"POST", "/api/projects/1", "GET, PUT, OPTIONS"},
		{"GET", "/api/sprints/1/tasks", "POST, OPTIONS"},
		{"DELETE", "/api/sprints/1/tasks", "POST, OPTIONS"},
		{"GET", "/api/sprints/1/tasks/2", "DELETE, OPTIONS"},
		{"PUT", "/api/sprints/1/tasks/2", "DELETE, OPTIONS"},
		{"PUT", "/api/sprints/1/status", "GET, OPTIONS"},
	} {
		rec := do(t, h, tc.method, tc.path, "")
		expectStatus(t, rec, http.StatusMethodNotAllowed)
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s Allow = %q", tc.method, tc.path, allow)
		}
		decode[errorResponse](t, rec)
	}
	for _, path := range []string{"/api/projects/1/tasks", "/api/sprints/1/tasks/2/x", "/api/projects/1/tasks/2"} {
		rec := do(t, h, "GET", path, "")
		expectStatus(t, rec, http.StatusNotFound)
		decode[errorResponse](t, rec)
	}
}

func TestV5CORSExposesTotal(t *testing.T) {
	h := newClockHandler(newFakeStore())
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "X-Total-Count" {
		t.Fatalf("expose = %q", got)
	}
	if rec.Header().Get(totalCountHeader) != "0" {
		t.Fatalf("total header = %q", rec.Header().Get(totalCountHeader))
	}

	req = httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Fatalf("exposed to disallowed origin")
	}
}

func TestV5InternalErrorDoesNotLeak(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("pq: secret_user")
	h := newClockHandler(fs)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/tasks?q=x&page=1", ""},
		{"GET", "/api/sprints?unassigned=true", ""},
		{"GET", "/api/projects?q=x", ""},
		{"GET", "/api/tasks/1/comments?page=1", ""},
		{"PUT", "/api/sprints/1", `{"name":"S","startDate":"2026-10-01","endDate":"2026-10-02","projectId":1}`},
		{"PUT", "/api/projects/1", `{"name":"P"}`},
		{"POST", "/api/sprints/1/tasks", `{"taskIds":[1]}`},
		{"DELETE", "/api/sprints/1/tasks/1", ""},
	} {
		rec := do(t, h, tc.method, tc.path, tc.body)
		expectStatus(t, rec, http.StatusInternalServerError)
		if strings.Contains(rec.Body.String(), "secret_user") {
			t.Fatalf("leaked: %s", rec.Body.String())
		}
		if rec.Header().Get(totalCountHeader) != "" {
			t.Fatalf("%s %s: total header on 500", tc.method, tc.path)
		}
	}
}

func TestChatSprintResolutionV5(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seedV5(t, h, fs)

	got := chat(t, h, `create task "x" for Alice due today in sprint "Sprint 1"`)
	if got.Changed || got.Task != nil ||
		got.Reply != `I couldn't create the task: sprint "Sprint 1" is ambiguous (Sprint 1 (Auth), Sprint 1 (Billing)).` {
		t.Fatalf("ambiguous: %+v", got)
	}
	got = chat(t, h, `create task "x" for Alice due today sprint spr`)
	if got.Changed || !strings.Contains(got.Reply, `(Sprint 1 (Auth), Sprint 1 (Billing))`) {
		t.Fatalf("ambiguous prefix: %+v", got)
	}
	if list := decode[[]taskView](t, do(t, h, "GET", "/api/tasks", "")); len(list) != 0 {
		t.Fatalf("ambiguous chat created %d tasks", len(list))
	}

	got = chat(t, h, `create task "Fix login" for Alice due today in sprint "Sprint 1" in project Billing`)
	want := `Created task #1 "Fix login" for Alice, due 2026-10-21, category Engineering, sprint Sprint 1, projects Billing.`
	if got.Reply != want || !got.Changed || !reflect.DeepEqual(got.Task.SprintIDs, []int64{2}) || !reflect.DeepEqual(got.Task.ProjectIDs, []int64{2}) {
		t.Fatalf("project-scoped: %+v %+v", got, got.Task)
	}

	expectStatus(t, do(t, h, "POST", "/api/sprints", `{"name":"Hardening","startDate":"2026-10-15","endDate":"2026-10-28","projectId":1}`), http.StatusCreated)
	got = chat(t, h, `create task "Fix logout" for Bob due today sprint hard`)
	want = `Created task #2 "Fix logout" for Bob, due 2026-10-21, category Engineering, sprint Hardening, projects Auth.`
	if got.Reply != want || !reflect.DeepEqual(got.Task.SprintIDs, []int64{4}) || !reflect.DeepEqual(got.Task.ProjectIDs, []int64{1}) {
		t.Fatalf("auto project: %+v %+v", got, got.Task)
	}

	got = chat(t, h, `create task "Fix more" for Bob due today sprint Hardening project Billing`)
	want = `Created task #3 "Fix more" for Bob, due 2026-10-21, category Engineering, sprint Hardening, projects Billing, Auth.`
	if got.Reply != want || !reflect.DeepEqual(got.Task.ProjectIDs, []int64{1, 2}) {
		t.Fatalf("fallback outside project: %+v %+v", got, got.Task)
	}

	got = chat(t, h, `create task "Old work" for Bob due today sprint old`)
	if !got.Changed || len(got.Task.ProjectIDs) != 0 || !strings.HasSuffix(got.Reply, "sprint Old.") {
		t.Fatalf("legacy sprint: %+v %+v", got, got.Task)
	}

	got = chat(t, h, `status of sprint "Sprint 1"`)
	if got.Status != nil || !strings.HasPrefix(got.Reply, `Sprint "Sprint 1" is ambiguous (Sprint 1 (Auth), Sprint 1 (Billing)). Which sprint`) {
		t.Fatalf("status ambiguous: %+v", got)
	}
	got = chat(t, h, `status of sprint hardening`)
	if got.Status == nil || !strings.HasPrefix(got.Reply, `Sprint "Hardening" is `) {
		t.Fatalf("status: %+v", got)
	}
}
