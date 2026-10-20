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

	"taskmanager/backend/internal/store"
)

const maxBodyBytes = 1 << 20

// TaskStore is the persistence contract the handlers depend on.
type TaskStore interface {
	List(ctx context.Context) ([]store.Task, error)
	Get(ctx context.Context, id int64) (store.Task, error)
	Create(ctx context.Context, in store.TaskInput) (store.Task, error)
	Update(ctx context.Context, id int64, in store.TaskInput) (store.Task, error)
	Delete(ctx context.Context, id int64) error
	ListComments(ctx context.Context, taskID int64) ([]store.Comment, error)
	CreateComment(ctx context.Context, taskID int64, in store.CommentInput) (store.Comment, error)
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
}

// NewHandler returns the API router wrapped in CORS middleware.
func NewHandler(s TaskStore, corsOrigin string, logger *slog.Logger) http.Handler {
	h := &handler{store: s, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks", h.list)
	mux.HandleFunc("POST /api/tasks", h.create)
	mux.HandleFunc("GET /api/tasks/{id}", h.get)
	mux.HandleFunc("PUT /api/tasks/{id}", h.update)
	mux.HandleFunc("DELETE /api/tasks/{id}", h.delete)
	mux.HandleFunc("GET /api/tasks/{id}/comments", h.listComments)
	mux.HandleFunc("POST /api/tasks/{id}/comments", h.createComment)
	mux.HandleFunc("/", notFoundOrMethodNotAllowed)
	return withCORS(corsOrigin, mux)
}

var (
	collectionPath = regexp.MustCompile(`^/api/tasks$`)
	itemPath       = regexp.MustCompile(`^/api/tasks/[^/]+$`)
	commentsPath   = regexp.MustCompile(`^/api/tasks/[^/]+/comments$`)
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
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.store.List(r.Context())
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toResponse(t))
	}
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, toResponse(t))
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeTask(w, r, false)
	if !ok {
		return
	}
	t, err := h.store.Create(r.Context(), in)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(t))
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
	writeJSON(w, http.StatusOK, toResponse(t))
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
	comments, err := h.store.ListComments(r.Context(), id)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	out := make([]commentResponse, 0, len(comments))
	for _, c := range comments {
		out = append(out, toCommentResponse(c))
	}
	writeJSON(w, http.StatusOK, out)
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

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	if !digitsOnly.MatchString(raw) {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
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
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only assignee, date, description and status") {
		return store.TaskInput{}, false
	}

	in, fields := req.validate(requireStatus)
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return store.TaskInput{}, false
	}
	return in, true
}

func (h *handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
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
