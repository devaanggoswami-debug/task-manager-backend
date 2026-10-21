package api

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"taskmanager/backend/internal/classify"
	"taskmanager/backend/internal/store"
)

const (
	maxAssigneeLen    = 100
	maxDescriptionLen = 1000
	maxAuthorLen      = 100
	maxCommentBodyLen = 2000
	maxNameLen        = 100
	maxChatMessageLen = 500
	dateLayout        = "2006-01-02"

	defaultStatus    = "not_started"
	invalidStatusMsg = "must be one of not_started, in_progress, blocked, done"
	invalidDateMsg   = "must be a valid date in YYYY-MM-DD format"
	invalidIDsMsg    = "must contain only positive integer ids"

	unknownProjectMsg = "unknown project"
	maxTaskIDs        = 100
)

var invalidCategoryMsg = "must be one of " + strings.Join(classify.Names, ", ")

var validStatuses = map[string]bool{
	"not_started": true,
	"in_progress": true,
	"blocked":     true,
	"done":        true,
}

// taskRequest is the body accepted by POST and PUT. Status and Category
// are pointers, and the id lists slices, so an omitted (or null) field can
// be told apart from an empty one.
type taskRequest struct {
	Assignee    string  `json:"assignee"`
	Date        string  `json:"date"`
	Description string  `json:"description"`
	Status      *string `json:"status"`
	Category    *string `json:"category"`
	SprintIDs   []int64 `json:"sprintIds"`
	ProjectIDs  []int64 `json:"projectIds"`
}

// validate trims and checks the request, returning the store input or a
// map of field name to error message. When requireStatus is false an
// omitted status defaults to not_started. An omitted category or id list
// stays nil; id lists are deduplicated and sorted.
func (r taskRequest) validate(requireStatus bool) (store.TaskInput, map[string]string) {
	fields := map[string]string{}

	assignee := strings.TrimSpace(r.Assignee)
	if msg := checkText(assignee, maxAssigneeLen); msg != "" {
		fields["assignee"] = msg
	}

	description := strings.TrimSpace(r.Description)
	if msg := checkText(description, maxDescriptionLen); msg != "" {
		fields["description"] = msg
	}

	var due time.Time
	if r.Date == "" {
		fields["date"] = "is required"
	} else if d, ok := parseDate(r.Date); !ok {
		fields["date"] = invalidDateMsg
	} else {
		due = d
	}

	status := defaultStatus
	switch {
	case r.Status != nil && !validStatuses[*r.Status]:
		fields["status"] = invalidStatusMsg
	case r.Status != nil:
		status = *r.Status
	case requireStatus:
		fields["status"] = "is required"
	}

	if r.Category != nil && !slices.Contains(classify.Names, *r.Category) {
		fields["category"] = invalidCategoryMsg
	}

	sprintIDs, ok := normalizeIDs(r.SprintIDs)
	if !ok {
		fields["sprintIds"] = invalidIDsMsg
	}
	projectIDs, ok := normalizeIDs(r.ProjectIDs)
	if !ok {
		fields["projectIds"] = invalidIDsMsg
	}

	if len(fields) > 0 {
		return store.TaskInput{}, fields
	}
	return store.TaskInput{
		Assignee: assignee, DueDate: due, Description: description, Status: status,
		Category: r.Category, SprintIDs: sprintIDs, ProjectIDs: projectIDs,
	}, nil
}

// normalizeIDs sorts and deduplicates ids, keeping nil as nil. It reports
// false when an id is not positive.
func normalizeIDs(ids []int64) ([]int64, bool) {
	if ids == nil {
		return nil, true
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > 0 && out[0] <= 0 {
		return nil, false
	}
	return out, true
}

// sprintRequest is the body accepted by POST /api/sprints and
// PUT /api/sprints/{id}.
type sprintRequest struct {
	Name      string `json:"name"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	ProjectID *int64 `json:"projectId"`
}

// validate trims and checks the request, returning the store input or a
// map of field name to error message.
func (r sprintRequest) validate() (store.SprintInput, map[string]string) {
	fields := map[string]string{}

	name := strings.TrimSpace(r.Name)
	if msg := checkText(name, maxNameLen); msg != "" {
		fields["name"] = msg
	}

	start, startOK := checkDate(r.StartDate, "startDate", fields)
	end, endOK := checkDate(r.EndDate, "endDate", fields)
	if startOK && endOK && end.Before(start) {
		fields["endDate"] = "must be on or after startDate"
	}

	switch {
	case r.ProjectID == nil:
		fields["projectId"] = "is required"
	case *r.ProjectID <= 0:
		fields["projectId"] = unknownProjectMsg
	}

	if len(fields) > 0 {
		return store.SprintInput{}, fields
	}
	return store.SprintInput{Name: name, StartDate: start, EndDate: end, ProjectID: r.ProjectID}, nil
}

// sprintTasksRequest is the body accepted by POST /api/sprints/{id}/tasks.
type sprintTasksRequest struct {
	TaskIDs []int64 `json:"taskIds"`
}

// validate checks for 1 to 100 positive ids, returning them deduplicated
// and sorted, or a map of field name to error message.
func (r sprintTasksRequest) validate() ([]int64, map[string]string) {
	switch {
	case len(r.TaskIDs) == 0:
		return nil, map[string]string{"taskIds": "is required"}
	case len(r.TaskIDs) > maxTaskIDs:
		return nil, map[string]string{"taskIds": "must contain at most " + strconv.Itoa(maxTaskIDs) + " ids"}
	}
	ids, ok := normalizeIDs(r.TaskIDs)
	if !ok {
		return nil, map[string]string{"taskIds": invalidIDsMsg}
	}
	return ids, nil
}

// projectRequest is the body accepted by POST /api/projects and
// PUT /api/projects/{id}.
type projectRequest struct {
	Name string `json:"name"`
}

// validate trims and checks the request, returning the store input or a
// map of field name to error message.
func (r projectRequest) validate() (store.ProjectInput, map[string]string) {
	name := strings.TrimSpace(r.Name)
	if msg := checkText(name, maxNameLen); msg != "" {
		return store.ProjectInput{}, map[string]string{"name": msg}
	}
	return store.ProjectInput{Name: name}, nil
}

// chatRequest is the body accepted by POST /api/chat.
type chatRequest struct {
	Message string `json:"message"`
}

// validate trims and checks the message.
func (r chatRequest) validate() (string, map[string]string) {
	msg := strings.TrimSpace(r.Message)
	if m := checkText(msg, maxChatMessageLen); m != "" {
		return "", map[string]string{"message": m}
	}
	return msg, nil
}

// checkDate parses a required date field, recording an error in fields
// under name when it is missing or invalid.
func checkDate(s, name string, fields map[string]string) (time.Time, bool) {
	if s == "" {
		fields[name] = "is required"
		return time.Time{}, false
	}
	d, ok := parseDate(s)
	if !ok {
		fields[name] = invalidDateMsg
	}
	return d, ok
}

// commentRequest is the body accepted by POST /api/tasks/{id}/comments.
type commentRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// validate trims and checks the request, returning the store input or a
// map of field name to error message.
func (r commentRequest) validate() (store.CommentInput, map[string]string) {
	fields := map[string]string{}

	author := strings.TrimSpace(r.Author)
	if msg := checkText(author, maxAuthorLen); msg != "" {
		fields["author"] = msg
	}

	body := strings.TrimSpace(r.Body)
	if msg := checkText(body, maxCommentBodyLen); msg != "" {
		fields["body"] = msg
	}

	if len(fields) > 0 {
		return store.CommentInput{}, fields
	}
	return store.CommentInput{Author: author, Body: body}, nil
}

func checkText(s string, maxLen int) string {
	switch {
	case s == "":
		return "is required"
	case strings.ContainsRune(s, 0):
		return "must not contain NUL characters"
	case utf8.RuneCountInString(s) > maxLen:
		return "must be at most " + strconv.Itoa(maxLen) + " characters"
	}
	return ""
}

// parseDate accepts only zero-padded YYYY-MM-DD strings naming a real
// calendar date; time.Parse rejects out-of-range days such as 2026-02-30.
func parseDate(s string) (time.Time, bool) {
	if len(s) != len(dateLayout) {
		return time.Time{}, false
	}
	d, err := time.Parse(dateLayout, s)
	if err != nil || d.Year() < 1 {
		return time.Time{}, false
	}
	return d, true
}
