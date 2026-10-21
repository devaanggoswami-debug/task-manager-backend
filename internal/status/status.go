// Package status derives an on-track / at-risk / blocked verdict for the
// tasks of a sprint or project.
package status

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind selects which rules apply.
type Kind int

const (
	Sprint Kind = iota
	Project
)

// Verdicts.
const (
	OnTrack = "on_track"
	AtRisk  = "at_risk"
	Blocked = "blocked"
)

const (
	maxExamples    = 3
	maxExampleLen  = 40
	maxElapsedLead = 25
)

// Task is the subset of a task the rules look at. DueDate is a calendar
// date; its clock time is ignored.
type Task struct {
	ID          int64
	Description string
	Status      string
	DueDate     time.Time
}

// Status is the outcome of Evaluate. Overdue counts open tasks due before
// today.
type Status struct {
	Status  string
	Reason  string
	Total   int
	Done    int
	Blocked int
	Overdue int
}

// Evaluate applies the status rules in order. sprintStart and sprintEnd
// are only used for Sprint. All dates are compared as calendar dates.
func Evaluate(kind Kind, tasks []Task, sprintStart, sprintEnd, today time.Time) Status {
	today = dateOnly(today)
	st := Status{Total: len(tasks)}
	if len(tasks) == 0 {
		st.Status, st.Reason = OnTrack, "no tasks yet"
		return st
	}

	var blocked, overdue []Task
	for _, t := range tasks {
		switch {
		case t.Status == "done":
			st.Done++
			continue
		case t.Status == "blocked":
			blocked = append(blocked, t)
		}
		if dateOnly(t.DueDate).Before(today) {
			overdue = append(overdue, t)
		}
	}
	st.Blocked, st.Overdue = len(blocked), len(overdue)
	open := st.Total - st.Done

	if len(blocked) > 0 {
		st.Status, st.Reason = Blocked, plural(len(blocked), "blocked task")+": "+examples(blocked)
		return st
	}
	if len(overdue) > 0 {
		st.Status, st.Reason = AtRisk, plural(len(overdue), "overdue task")+": "+examples(overdue)
		return st
	}

	if kind == Sprint {
		start, end := dateOnly(sprintStart), dateOnly(sprintEnd)
		if end.Before(today) && open > 0 {
			st.Status = AtRisk
			st.Reason = fmt.Sprintf("sprint ended on %s with %s", end.Format("2006-01-02"), plural(open, "open task"))
			return st
		}
		late := 0
		for _, t := range tasks {
			if t.Status != "done" && dateOnly(t.DueDate).After(end) {
				late++
			}
		}
		if late > 0 {
			st.Status, st.Reason = AtRisk, plural(late, "task")+" due after sprint end"
			return st
		}

		length := days(start, end) + 1
		if !today.Before(start) {
			elapsed := min(days(start, today)+1, length)
			// elapsed/length - done/total > 25/100, in exact integer arithmetic.
			if 100*(elapsed*st.Total-st.Done*length) > maxElapsedLead*length*st.Total {
				st.Status = AtRisk
				st.Reason = fmt.Sprintf("%d%% of time elapsed but only %d%% of tasks done",
					100*elapsed/length, 100*st.Done/st.Total)
				return st
			}
		}

		left := min(max(days(today, end)+1, 0), length)
		st.Status = OnTrack
		st.Reason = fmt.Sprintf("%d of %d tasks done, %s left", st.Done, st.Total, plural(left, "day"))
		return st
	}

	st.Status = OnTrack
	st.Reason = fmt.Sprintf("%d of %d tasks done", st.Done, st.Total)
	return st
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func days(from, to time.Time) int {
	return int(to.Sub(from).Hours() / 24)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func examples(tasks []Task) string {
	parts := make([]string, 0, maxExamples)
	for i, t := range tasks {
		if i == maxExamples {
			break
		}
		parts = append(parts, fmt.Sprintf("#%d %q", t.ID, Truncate(t.Description, maxExampleLen)))
	}
	out := strings.Join(parts, ", ")
	if extra := len(tasks) - maxExamples; extra > 0 {
		out += fmt.Sprintf(" and %d more", extra)
	}
	return out
}

// Truncate shortens s to at most n characters, marking a cut with "…".
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
