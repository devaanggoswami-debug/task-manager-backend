package api

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskmanager/backend/internal/gemini"
	"taskmanager/backend/internal/store"
)

// Assistant generates a natural-language reply to a free-form question, given
// read-only context about the user's tasks. It is implemented by
// *gemini.Client. The implementation must not perform any action: its output
// is returned to the user as text and never drives a query or mutation.
type Assistant interface {
	Answer(ctx context.Context, systemInstruction, question string) (string, error)
}

const (
	// assistantTimeout bounds a single assistant turn. It is kept below the
	// server's WriteTimeout so the handler can still write a reply. It must be
	// large enough for the Gemini client's bounded retries (up to 3 attempts
	// at a 15s per-attempt timeout, plus short backoff waits) to complete.
	assistantTimeout = 55 * time.Second

	// assistantMaxTasks bounds how many tasks are packed into the model
	// context, keeping the request small and predictable.
	assistantMaxTasks = 50
)

// assistantSystemPreamble states the guardrails. The task context is appended
// to it by assistantContext.
const assistantSystemPreamble = `You are the built-in assistant of a task-management web app.
Answer the user's question about their tasks, projects, sprints, due dates and statuses using ONLY the data in the "Context" section below.

Rules:
- Be concise and factual. Reply in plain text (no markdown tables or code blocks).
- If the context does not contain the answer, say you don't have that information. Do not guess or invent tasks, people, dates, or statuses.
- You cannot create, edit, delete, or change anything. If the user asks you to, explain that they can create a task or ask for status directly, for example:
  - ` + chatExampleCreate + `
  - ` + chatExampleStatus + `
- Status meanings: a task is "blocked" when its status is blocked; a task is "overdue" when it is not done and its due date is before today.`

// chatAssistant answers a free-form message with the configured assistant.
// When no assistant is configured it returns the static help reply, so the
// endpoint keeps working without a Gemini key. Assistant or API errors become
// a friendly reply with HTTP 200 (changed=false); only a failure to build the
// context (a store error) is returned as an error for a 500.
func (h *handler) chatAssistant(ctx context.Context, msg string) (chatResponse, error) {
	if h.assistant == nil {
		return chatResponse{Reply: helpReply}, nil
	}

	instruction, err := h.assistantContext(ctx)
	if err != nil {
		return chatResponse{}, err
	}

	answerCtx, cancel := context.WithTimeout(ctx, assistantTimeout)
	defer cancel()
	reply, err := h.assistant.Answer(answerCtx, instruction, msg)
	if err != nil {
		// Log the detail for operators; the key is never part of the error.
		h.logger.Warn("assistant request failed", "err", err)
		return chatResponse{Reply: assistantErrorReply(err)}, nil
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return chatResponse{Reply: assistantUnavailableReply()}, nil
	}
	return chatResponse{Reply: reply}, nil
}

// assistantErrorReply maps an assistant error to a user-friendly reply. It
// never switches to a paid model or project; a rate limit simply asks the
// user to wait.
func assistantErrorReply(err error) string {
	switch {
	case errors.Is(err, gemini.ErrRateLimited):
		return "The assistant has reached its request limit for now. Please wait a minute and try again."
	case errors.Is(err, gemini.ErrTimeout):
		return "The assistant took too long to respond. Please try again."
	default:
		return assistantUnavailableReply()
	}
}

func assistantUnavailableReply() string {
	return "The assistant is temporarily unavailable. You can still create tasks and ask about status directly:\n" +
		"- " + chatExampleCreate + "\n" +
		"- " + chatExampleStatus
}

// assistantContext builds the read-only "Context" block: today's date, the
// projects and sprints, and up to assistantMaxTasks tasks (ordered by due
// date). It is appended to the system preamble.
func (h *handler) assistantContext(ctx context.Context) (string, error) {
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	sprints, err := h.store.ListSprints(ctx)
	if err != nil {
		return "", err
	}
	tasks, err := h.store.ListFiltered(ctx, store.TaskFilter{})
	if err != nil {
		return "", err
	}

	projectName := make(map[int64]string, len(projects))
	for _, p := range projects {
		projectName[p.ID] = p.Name
	}
	sprintName := make(map[int64]string, len(sprints))
	for _, s := range sprints {
		sprintName[s.ID] = s.Name
	}

	var b strings.Builder
	b.WriteString(assistantSystemPreamble)
	b.WriteString("\n\nContext:\nToday is ")
	b.WriteString(h.today().Format(dateLayout))
	b.WriteString(".\n")

	b.WriteString("\nProjects (" + strconv.Itoa(len(projects)) + "):\n")
	for _, p := range projects {
		b.WriteString("- #" + strconv.FormatInt(p.ID, 10) + " " + p.Name +
			" — " + strconv.Itoa(p.TaskCount) + " tasks, " + strconv.Itoa(p.SprintCount) + " sprints\n")
	}

	b.WriteString("\nSprints (" + strconv.Itoa(len(sprints)) + "):\n")
	for _, s := range sprints {
		owner := "unassigned"
		if s.ProjectID != nil {
			if name := projectName[*s.ProjectID]; name != "" {
				owner = "project " + name
			}
		}
		b.WriteString("- #" + strconv.FormatInt(s.ID, 10) + " \"" + s.Name + "\" (" + owner + ") " +
			s.StartDate.Format(dateLayout) + " to " + s.EndDate.Format(dateLayout) +
			" — " + strconv.Itoa(s.TaskCount) + " tasks\n")
	}

	shown := tasks
	truncated := false
	if len(shown) > assistantMaxTasks {
		shown = shown[:assistantMaxTasks]
		truncated = true
	}
	header := "\nTasks (" + strconv.Itoa(len(tasks)) + ")"
	if truncated {
		header = "\nTasks (showing first " + strconv.Itoa(assistantMaxTasks) + " of " + strconv.Itoa(len(tasks)) + ")"
	}
	b.WriteString(header + ", each as #id [status] \"description\" — assignee, due date, category, sprints, projects:\n")
	for _, t := range shown {
		b.WriteString("- #" + strconv.FormatInt(t.ID, 10) + " [" + t.Status + "] \"" + t.Description + "\" — " + t.Assignee +
			", due " + t.DueDate.Format(dateLayout) +
			", " + category(t.Category) +
			", sprints " + names(t.SprintIDs, sprintName) +
			", projects " + names(t.ProjectIDs, projectName) + "\n")
	}
	return b.String(), nil
}

func category(c *string) string {
	if c == nil || *c == "" {
		return "unclassified"
	}
	return *c
}

// names maps ids to their names (sorted), or "none" when there are none.
func names(ids []int64, byID map[int64]string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name := byID[id]; name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
