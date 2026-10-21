// Package api implements the task manager HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"taskmanager/backend/internal/classify"
	"taskmanager/backend/internal/status"
	"taskmanager/backend/internal/store"
)

const maxBodyBytes = 1 << 20

// TaskStore is the persistence contract the handlers depend on. The *Page
// methods also return the total number of matching items.
type TaskStore interface {
	ListFiltered(ctx context.Context, f store.TaskFilter) ([]store.Task, error)
	ListTasksPage(ctx context.Context, f store.TaskFilter) ([]store.Task, int, error)
	Get(ctx context.Context, id int64) (store.Task, error)
	Create(ctx context.Context, in store.TaskInput) (store.Task, error)
	Update(ctx context.Context, id int64, in store.TaskInput) (store.Task, error)
	Delete(ctx context.Context, id int64) error
	ListCommentsPage(ctx context.Context, taskID int64, p store.Page) ([]store.Comment, int, error)
	CreateComment(ctx context.Context, taskID int64, in store.CommentInput) (store.Comment, error)
	CreateSprint(ctx context.Context, in store.SprintInput) (store.Sprint, error)
	UpdateSprint(ctx context.Context, id int64, in store.SprintInput) (store.Sprint, error)
	ListSprints(ctx context.Context) ([]store.Sprint, error)
	ListSprintsPage(ctx context.Context, f store.SprintFilter) ([]store.Sprint, int, error)
	GetSprint(ctx context.Context, id int64) (store.Sprint, error)
	AddTasksToSprint(ctx context.Context, sprintID int64, taskIDs []int64) (store.Sprint, error)
	RemoveTaskFromSprint(ctx context.Context, sprintID, taskID int64) error
	CreateProject(ctx context.Context, in store.ProjectInput) (store.Project, error)
	UpdateProject(ctx context.Context, id int64, in store.ProjectInput) (store.Project, error)
	ListProjects(ctx context.Context) ([]store.Project, error)
	ListProjectsPage(ctx context.Context, f store.ProjectFilter) ([]store.Project, int, error)
	GetProject(ctx context.Context, id int64) (store.Project, error)
}

type taskResponse struct {
	ID          int64  `json:"id"`
	Assignee    string `json:"assignee"`
	Date        string `json:"date"`
	Description string `json:"description"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// taskView is the full task JSON: the v3 fields plus category and
// memberships.
type taskView struct {
	taskResponse
	Category   *string `json:"category"`
	SprintIDs  []int64 `json:"sprintIds"`
	ProjectIDs []int64 `json:"projectIds"`
}

type sprintResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	ProjectID *int64 `json:"projectId"`
	TaskCount int    `json:"taskCount"`
	CreatedAt string `json:"createdAt"`
}

type projectResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	TaskCount   int    `json:"taskCount"`
	SprintCount int    `json:"sprintCount"`
	CreatedAt   string `json:"createdAt"`
}

type statusResponse struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Blocked int    `json:"blocked"`
	Overdue int    `json:"overdue"`
}

type commentResponse struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"taskId"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type errorResponse struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields,omitempty"`
}

func toResponse(t store.Task) taskResponse {
	return taskResponse{
		ID:          t.ID,
		Assignee:    t.Assignee,
		Date:        t.DueDate.Format(dateLayout),
		Description: t.Description,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toView(t store.Task) taskView {
	v := taskView{taskResponse: toResponse(t), Category: t.Category, SprintIDs: t.SprintIDs, ProjectIDs: t.ProjectIDs}
	if v.SprintIDs == nil {
		v.SprintIDs = []int64{}
	}
	if v.ProjectIDs == nil {
		v.ProjectIDs = []int64{}
	}
	return v
}

func toSprintResponse(s store.Sprint) sprintResponse {
	return sprintResponse{
		ID:        s.ID,
		Name:      s.Name,
		StartDate: s.StartDate.Format(dateLayout),
		EndDate:   s.EndDate.Format(dateLayout),
		ProjectID: s.ProjectID,
		TaskCount: s.TaskCount,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toProjectResponse(p store.Project) projectResponse {
	return projectResponse{ID: p.ID, Name: p.Name, TaskCount: p.TaskCount, SprintCount: p.SprintCount,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339)}
}

func toStatusResponse(s status.Status) statusResponse {
	return statusResponse{Status: s.Status, Reason: s.Reason, Total: s.Total, Done: s.Done, Blocked: s.Blocked, Overdue: s.Overdue}
}

func toCommentResponse(c store.Comment) commentResponse {
	return commentResponse{
		ID:        c.ID,
		TaskID:    c.TaskID,
		Author:    c.Author,
		Body:      c.Body,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type handler struct {
	store  TaskStore
	logger *slog.Logger
	now    func() time.Time
}

// NewHandler returns the API router wrapped in CORS middleware.
func NewHandler(s TaskStore, corsOrigin string, logger *slog.Logger) http.Handler {
	return newHandler(s, corsOrigin, logger, time.Now)
}

// newHandler is NewHandler with an injectable clock; "today" is the
// calendar date of now() in its own location.
func newHandler(s TaskStore, corsOrigin string, logger *slog.Logger, now func() time.Time) http.Handler {
	h := &handler{store: s, logger: logger, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks", h.list)
	mux.HandleFunc("POST /api/tasks", h.create)
	mux.HandleFunc("GET /api/tasks/{id}", h.get)
	mux.HandleFunc("PUT /api/tasks/{id}", h.update)
	mux.HandleFunc("DELETE /api/tasks/{id}", h.delete)
	mux.HandleFunc("GET /api/tasks/{id}/comments", h.listComments)
	mux.HandleFunc("POST /api/tasks/{id}/comments", h.createComment)
	mux.HandleFunc("GET /api/sprints", h.listSprints)
	mux.HandleFunc("POST /api/sprints", h.createSprint)
	mux.HandleFunc("GET /api/sprints/{id}", h.getSprint)
	mux.HandleFunc("PUT /api/sprints/{id}", h.updateSprint)
	mux.HandleFunc("GET /api/sprints/{id}/status", h.sprintStatus)
	mux.HandleFunc("POST /api/sprints/{id}/tasks", h.addSprintTasks)
	mux.HandleFunc("DELETE /api/sprints/{id}/tasks/{taskId}", h.removeSprintTask)
	mux.HandleFunc("GET /api/projects", h.listProjects)
	mux.HandleFunc("POST /api/projects", h.createProject)
	mux.HandleFunc("GET /api/projects/{id}", h.getProject)
	mux.HandleFunc("PUT /api/projects/{id}", h.updateProject)
	mux.HandleFunc("GET /api/projects/{id}/status", h.projectStatus)
	mux.HandleFunc("POST /api/chat", h.chat)
	mux.HandleFunc("/", notFoundOrMethodNotAllowed)
	return withCORS(corsOrigin, mux)
}

var (
	collectionPath  = regexp.MustCompile(`^/api/(tasks|sprints|projects)$`)
	itemPath        = regexp.MustCompile(`^/api/tasks/[^/]+$`)
	commentsPath    = regexp.MustCompile(`^/api/tasks/[^/]+/comments$`)
	editablePath    = regexp.MustCompile(`^/api/(sprints|projects)/[^/]+$`)
	statusPath      = regexp.MustCompile(`^/api/(sprints|projects)/[^/]+/status$`)
	sprintTasksPath = regexp.MustCompile(`^/api/sprints/[^/]+/tasks$`)
	sprintTaskPath  = regexp.MustCompile(`^/api/sprints/[^/]+/tasks/[^/]+$`)
	chatPath        = regexp.MustCompile(`^/api/chat$`)
)

// notFoundOrMethodNotAllowed replaces the mux's plain-text fallbacks with
// JSON responses.
func notFoundOrMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	switch {
	case collectionPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case itemPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "GET, PUT, DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case commentsPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case editablePath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "GET, PUT, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case statusPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "GET, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case sprintTasksPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case sprintTaskPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case chatPath.MatchString(r.URL.Path):
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	f, ok := parseTaskFilter(w, r)
	if !ok {
		return
	}
	tasks, total, err := h.store.ListTasksPage(r.Context(), f)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toView(t))
	}
	writeList(w, out, total)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	t, err := h.store.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toView(t))
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeTask(w, r, false)
	if !ok {
		return
	}
	if in.Category == nil {
		category, _ := classify.Classify(in.Description)
		in.Category = &category
	}
	t, err := h.store.Create(r.Context(), in)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toView(t))
}

func (h *handler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	in, ok := decodeTask(w, r, true)
	if !ok {
		return
	}
	t, err := h.store.Update(r.Context(), id, in)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toView(t))
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.store.Delete(r.Context(), id); err != nil {
		h.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) listComments(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	page, ok := parsePageOnly(w, r)
	if !ok {
		return
	}
	comments, total, err := h.store.ListCommentsPage(r.Context(), id, page)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	out := make([]commentResponse, 0, len(comments))
	for _, c := range comments {
		out = append(out, toCommentResponse(c))
	}
	writeList(w, out, total)
}

func (h *handler) createComment(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req commentRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only author and body") {
		return
	}
	in, fields := req.validate()
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return
	}
	c, err := h.store.CreateComment(r.Context(), id, in)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCommentResponse(c))
}

func (h *handler) listSprints(w http.ResponseWriter, r *http.Request) {
	f, ok := parseSprintFilter(w, r)
	if !ok {
		return
	}
	sprints, total, err := h.store.ListSprintsPage(r.Context(), f)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := make([]sprintResponse, 0, len(sprints))
	for _, s := range sprints {
		out = append(out, toSprintResponse(s))
	}
	writeList(w, out, total)
}

// decodeSprint decodes and validates a sprint body, writing the error
// response itself when it returns false.
func decodeSprint(w http.ResponseWriter, r *http.Request) (store.SprintInput, bool) {
	var req sprintRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only name, startDate, endDate and projectId") {
		return store.SprintInput{}, false
	}
	in, fields := req.validate()
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return store.SprintInput{}, false
	}
	return in, true
}

// sprintWriteError maps sprint write errors: a duplicate name to 409, an
// unknown project to a 400 validation failure and a missing sprint to 404.
func (h *handler) sprintWriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "a sprint with that name already exists")
	case errors.Is(err, store.ErrUnknownProject):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: map[string]string{"projectId": unknownProjectMsg}})
	default:
		h.lookupError(w, r, err, "sprint not found")
	}
}

func (h *handler) createSprint(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeSprint(w, r)
	if !ok {
		return
	}
	s, err := h.store.CreateSprint(r.Context(), in)
	if err != nil {
		h.sprintWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSprintResponse(s))
}

func (h *handler) updateSprint(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	in, ok := decodeSprint(w, r)
	if !ok {
		return
	}
	s, err := h.store.UpdateSprint(r.Context(), id, in)
	if err != nil {
		h.sprintWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSprintResponse(s))
}

func (h *handler) addSprintTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req sprintTasksRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only taskIds") {
		return
	}
	taskIDs, fields := req.validate()
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return
	}
	s, err := h.store.AddTasksToSprint(r.Context(), id, taskIDs)
	var unknown *store.UnknownIDsError
	switch {
	case errors.As(err, &unknown):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed",
			Fields: map[string]string{"taskIds": "unknown id(s): " + store.JoinIDs(unknown.TaskIDs)}})
	case err != nil:
		h.lookupError(w, r, err, "sprint not found")
	default:
		writeJSON(w, http.StatusOK, toSprintResponse(s))
	}
}

func (h *handler) removeSprintTask(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	taskID, ok := parsePathID(w, r, "taskId")
	if !ok {
		return
	}
	err := h.store.RemoveTaskFromSprint(r.Context(), id, taskID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrSprintNotFound), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotInSprint):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		h.internalError(w, r, err)
	}
}

func (h *handler) getSprint(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	s, err := h.store.GetSprint(r.Context(), id)
	if err != nil {
		h.lookupError(w, r, err, "sprint not found")
		return
	}
	writeJSON(w, http.StatusOK, toSprintResponse(s))
}

func (h *handler) sprintStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	s, err := h.store.GetSprint(r.Context(), id)
	if err != nil {
		h.lookupError(w, r, err, "sprint not found")
		return
	}
	st, err := h.evaluateSprint(r.Context(), s)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toStatusResponse(st))
}

func (h *handler) listProjects(w http.ResponseWriter, r *http.Request) {
	f, ok := parseProjectFilter(w, r)
	if !ok {
		return
	}
	projects, total, err := h.store.ListProjectsPage(r.Context(), f)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := make([]projectResponse, 0, len(projects))
	for _, p := range projects {
		out = append(out, toProjectResponse(p))
	}
	writeList(w, out, total)
}

// decodeProject decodes and validates a project body, writing the error
// response itself when it returns false.
func decodeProject(w http.ResponseWriter, r *http.Request) (store.ProjectInput, bool) {
	var req projectRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only name") {
		return store.ProjectInput{}, false
	}
	in, fields := req.validate()
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return store.ProjectInput{}, false
	}
	return in, true
}

func (h *handler) createProject(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeProject(w, r)
	if !ok {
		return
	}
	p, err := h.store.CreateProject(r.Context(), in)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a project with that name already exists")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProjectResponse(p))
}

func (h *handler) updateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	in, ok := decodeProject(w, r)
	if !ok {
		return
	}
	p, err := h.store.UpdateProject(r.Context(), id, in)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a project with that name already exists")
		return
	}
	if err != nil {
		h.lookupError(w, r, err, "project not found")
		return
	}
	writeJSON(w, http.StatusOK, toProjectResponse(p))
}

func (h *handler) getProject(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.lookupError(w, r, err, "project not found")
		return
	}
	writeJSON(w, http.StatusOK, toProjectResponse(p))
}

func (h *handler) projectStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.lookupError(w, r, err, "project not found")
		return
	}
	st, err := h.evaluateProject(r.Context(), p)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toStatusResponse(st))
}

func (h *handler) today() time.Time {
	now := h.now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func (h *handler) evaluateSprint(ctx context.Context, s store.Sprint) (status.Status, error) {
	tasks, err := h.store.ListFiltered(ctx, store.TaskFilter{SprintID: s.ID})
	if err != nil {
		return status.Status{}, err
	}
	return status.Evaluate(status.Sprint, statusTasks(tasks), s.StartDate, s.EndDate, h.today()), nil
}

func (h *handler) evaluateProject(ctx context.Context, p store.Project) (status.Status, error) {
	tasks, err := h.store.ListFiltered(ctx, store.TaskFilter{ProjectID: p.ID})
	if err != nil {
		return status.Status{}, err
	}
	return status.Evaluate(status.Project, statusTasks(tasks), time.Time{}, time.Time{}, h.today()), nil
}

func statusTasks(tasks []store.Task) []status.Task {
	out := make([]status.Task, len(tasks))
	for i, t := range tasks {
		out[i] = status.Task{ID: t.ID, Description: t.Description, Status: t.Status, DueDate: t.DueDate}
	}
	return out
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return parsePathID(w, r, "id")
}

func parsePathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, ok := parsePositive(r.PathValue(name))
	if !ok {
		writeError(w, http.StatusBadRequest, name+" must be a positive integer")
	}
	return id, ok
}

// parseQueryID reads an optional positive id query parameter, returning 0
// when it is absent.
func parseQueryID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	q := r.URL.Query()
	if !q.Has(name) {
		return 0, true
	}
	id, ok := parsePositive(q.Get(name))
	if !ok {
		writeError(w, http.StatusBadRequest, name+" must be a positive integer")
	}
	return id, ok
}

func parsePositive(raw string) (int64, bool) {
	if !digitsOnly.MatchString(raw) {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// decodeJSON reads exactly one JSON object with no unknown fields from a
// body capped at 1 MiB into dst. shapeMsg is the 400 message for a body
// that is not such an object. It writes the error response itself when it
// returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, shapeMsg string) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusBadRequest, "Content-Type must be application/json")
		return false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body must not exceed 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, shapeMsg)
		}
		return false
	}
	return true
}

// decodeTask decodes and validates a task body, writing the error response
// itself when it returns false.
func decodeTask(w http.ResponseWriter, r *http.Request, requireStatus bool) (store.TaskInput, bool) {
	var req taskRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only assignee, date, description, status, category, sprintIds and projectIds") {
		return store.TaskInput{}, false
	}

	in, fields := req.validate(requireStatus)
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return store.TaskInput{}, false
	}
	return in, true
}

// storeError maps task store errors: a missing task to 404 and unknown
// membership ids to a 400 validation failure.
func (h *handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	var unknown *store.UnknownIDsError
	if errors.As(err, &unknown) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: unknownIDFields(unknown)})
		return
	}
	h.lookupError(w, r, err, "task not found")
}

func unknownIDFields(e *store.UnknownIDsError) map[string]string {
	fields := map[string]string{}
	if len(e.SprintIDs) > 0 {
		fields["sprintIds"] = "unknown id(s): " + store.JoinIDs(e.SprintIDs)
	}
	if len(e.ProjectIDs) > 0 {
		fields["projectIds"] = "unknown id(s): " + store.JoinIDs(e.ProjectIDs)
	}
	return fields
}

func (h *handler) lookupError(w http.ResponseWriter, r *http.Request, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	h.internalError(w, r, err)
}

func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
