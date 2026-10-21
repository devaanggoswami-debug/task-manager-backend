package api

import (
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"taskmanager/backend/internal/classify"
	"taskmanager/backend/internal/store"
)

const (
	totalCountHeader = "X-Total-Count"
	defaultPageSize  = 20
	maxPageSize      = 100
	maxQueryLen      = 100

	invalidPageMsg     = "must be a positive integer"
	invalidPageSizeMsg = "must be an integer between 1 and 100"
	invalidBoolMsg     = "must be true or false"
)

var (
	filterCategories         = append(slices.Clone(classify.Names), store.Unclassified)
	invalidFilterCategoryMsg = "must be one of " + strings.Join(filterCategories, ", ")
)

// parsePage reads the optional page and pageSize parameters. When both are
// absent it returns the zero Page (the whole list); otherwise missing
// values default to page 1 and pageSize 20. Invalid values are recorded in
// fields.
func parsePage(q url.Values, fields map[string]string) store.Page {
	if !q.Has("page") && !q.Has("pageSize") {
		return store.Page{}
	}
	page, size := int64(1), int64(defaultPageSize)
	if q.Has("page") {
		v, ok := parsePositive(q.Get("page"))
		if !ok {
			fields["page"] = invalidPageMsg
		}
		page = v
	}
	if q.Has("pageSize") {
		v, ok := parsePositive(q.Get("pageSize"))
		if !ok || v > maxPageSize {
			fields["pageSize"] = invalidPageSizeMsg
		}
		size = v
	}
	if fields["page"] != "" || fields["pageSize"] != "" {
		return store.Page{}
	}
	offset := int64(math.MaxInt64)
	if page-1 <= math.MaxInt64/size {
		offset = (page - 1) * size
	}
	return store.Page{Limit: size, Offset: offset}
}

// parseEnum reads an optional parameter that must be one of allowed,
// returning "" when it is absent.
func parseEnum(q url.Values, name string, allowed []string, msg string, fields map[string]string) string {
	if !q.Has(name) {
		return ""
	}
	v := q.Get(name)
	if !slices.Contains(allowed, v) {
		fields[name] = msg
		return ""
	}
	return v
}

// parseSearch reads the optional q parameter, trimmed. Blank means no
// search.
func parseSearch(q url.Values, fields map[string]string) string {
	s := strings.TrimSpace(q.Get("q"))
	if s == "" {
		return ""
	}
	if msg := checkText(s, maxQueryLen); msg != "" {
		fields["q"] = msg
		return ""
	}
	return s
}

// parseBool reads an optional true/false parameter.
func parseBool(q url.Values, name string, fields map[string]string) bool {
	if !q.Has(name) {
		return false
	}
	switch q.Get(name) {
	case "true":
		return true
	case "false":
		return false
	}
	fields[name] = invalidBoolMsg
	return false
}

var statusNames = []string{"not_started", "in_progress", "blocked", "done"}

// parseTaskFilter reads the GET /api/tasks query, writing the error
// response itself when it returns false.
func parseTaskFilter(w http.ResponseWriter, r *http.Request) (store.TaskFilter, bool) {
	var f store.TaskFilter
	var ok bool
	if f.SprintID, ok = parseQueryID(w, r, "sprintId"); !ok {
		return f, false
	}
	if f.ProjectID, ok = parseQueryID(w, r, "projectId"); !ok {
		return f, false
	}
	if f.ExcludeSprintID, ok = parseQueryID(w, r, "excludeSprintId"); !ok {
		return f, false
	}
	q := r.URL.Query()
	fields := map[string]string{}
	f.Status = parseEnum(q, "status", statusNames, invalidStatusMsg, fields)
	f.Category = parseEnum(q, "category", filterCategories, invalidFilterCategoryMsg, fields)
	f.Q = parseSearch(q, fields)
	f.Page = parsePage(q, fields)
	return f, checkQueryFields(w, fields)
}

// parseSprintFilter reads the GET /api/sprints query, writing the error
// response itself when it returns false.
func parseSprintFilter(w http.ResponseWriter, r *http.Request) (store.SprintFilter, bool) {
	var f store.SprintFilter
	var ok bool
	if f.ProjectID, ok = parseQueryID(w, r, "projectId"); !ok {
		return f, false
	}
	q := r.URL.Query()
	fields := map[string]string{}
	f.Unassigned = parseBool(q, "unassigned", fields)
	f.Page = parsePage(q, fields)
	return f, checkQueryFields(w, fields)
}

// parseProjectFilter reads the GET /api/projects query, writing the error
// response itself when it returns false.
func parseProjectFilter(w http.ResponseWriter, r *http.Request) (store.ProjectFilter, bool) {
	q := r.URL.Query()
	fields := map[string]string{}
	f := store.ProjectFilter{Q: parseSearch(q, fields), Page: parsePage(q, fields)}
	return f, checkQueryFields(w, fields)
}

// parsePageOnly reads just page and pageSize, writing the error response
// itself when it returns false.
func parsePageOnly(w http.ResponseWriter, r *http.Request) (store.Page, bool) {
	fields := map[string]string{}
	p := parsePage(r.URL.Query(), fields)
	return p, checkQueryFields(w, fields)
}

func checkQueryFields(w http.ResponseWriter, fields map[string]string) bool {
	if len(fields) == 0 {
		return true
	}
	writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
	return false
}

// writeList writes a 200 JSON array with the total matching count.
func writeList[T any](w http.ResponseWriter, items []T, total int) {
	w.Header().Set(totalCountHeader, strconv.Itoa(total))
	writeJSON(w, http.StatusOK, items)
}
