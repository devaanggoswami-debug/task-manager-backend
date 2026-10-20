package api

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"taskmanager/backend/internal/store"
)

const (
	maxAssigneeLen    = 100
	maxDescriptionLen = 1000
	maxAuthorLen      = 100
	maxCommentBodyLen = 2000
	dateLayout        = "2006-01-02"

	defaultStatus    = "not_started"
	invalidStatusMsg = "must be one of not_started, in_progress, blocked, done"
)

var validStatuses = map[string]bool{
	"not_started": true,
	"in_progress": true,
	"blocked":     true,
	"done":        true,
}

// taskRequest is the body accepted by POST and PUT. Status is a pointer so
// an omitted field can be told apart from an empty one.
type taskRequest struct {
	Assignee    string  `json:"assignee"`
	Date        string  `json:"date"`
	Description string  `json:"description"`
	Status      *string `json:"status"`
}

// validate trims and checks the request, returning the store input or a
// map of field name to error message. When requireStatus is false an
// omitted status defaults to not_started.
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
		fields["date"] = "must be a valid date in YYYY-MM-DD format"
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

	if len(fields) > 0 {
		return store.TaskInput{}, fields
	}
	return store.TaskInput{Assignee: assignee, DueDate: due, Description: description, Status: status}, nil
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
