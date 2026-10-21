package status

import (
	"strings"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func task(id int64, status, due string) Task {
	return Task{ID: id, Description: "task " + string(rune('a'+id-1)), Status: status, DueDate: d(due)}
}

func TestEvaluate(t *testing.T) {
	today := d("2026-10-21")
	long := strings.Repeat("x", 50)

	tests := []struct {
		name       string
		kind       Kind
		tasks      []Task
		start, end string
		want       Status
	}{
		{
			name: "empty sprint", kind: Sprint, start: "2026-10-15", end: "2026-10-28",
			want: Status{Status: OnTrack, Reason: "no tasks yet"},
		},
		{
			name: "empty project", kind: Project,
			want: Status{Status: OnTrack, Reason: "no tasks yet"},
		},
		{
			name: "blocked names up to 3 and truncates", kind: Project,
			tasks: []Task{
				{ID: 1, Description: long, Status: "blocked", DueDate: d("2026-11-01")},
				task(2, "blocked", "2026-11-01"), task(3, "blocked", "2026-11-01"), task(4, "blocked", "2026-11-01"),
				task(5, "done", "2026-11-01"),
			},
			want: Status{Status: Blocked, Total: 5, Done: 1, Blocked: 4,
				Reason: `4 blocked tasks: #1 "` + strings.Repeat("x", 39) + `…", #2 "task b", #3 "task c" and 1 more`},
		},
		{
			name: "blocked beats overdue", kind: Sprint, start: "2026-10-15", end: "2026-10-28",
			tasks: []Task{task(1, "in_progress", "2026-10-20"), task(2, "blocked", "2026-10-25")},
			want:  Status{Status: Blocked, Total: 2, Blocked: 1, Overdue: 1, Reason: `1 blocked task: #2 "task b"`},
		},
		{
			name: "overdue", kind: Project,
			tasks: []Task{task(1, "not_started", "2026-10-20"), task(2, "in_progress", "2026-10-01"), task(3, "done", "2026-10-01")},
			want:  Status{Status: AtRisk, Total: 3, Done: 1, Overdue: 2, Reason: `2 overdue tasks: #1 "task a", #2 "task b"`},
		},
		{
			name: "due today is not overdue", kind: Project,
			tasks: []Task{task(1, "not_started", "2026-10-21")},
			want:  Status{Status: OnTrack, Total: 1, Reason: "0 of 1 tasks done"},
		},
		{
			name: "done task in the past is not overdue", kind: Project,
			tasks: []Task{task(1, "done", "2026-01-01")},
			want:  Status{Status: OnTrack, Total: 1, Done: 1, Reason: "1 of 1 tasks done"},
		},
		{
			name: "project ignores sprint rules", kind: Project, start: "2026-10-01", end: "2026-10-02",
			tasks: []Task{task(1, "not_started", "2027-01-01")},
			want:  Status{Status: OnTrack, Total: 1, Reason: "0 of 1 tasks done"},
		},
		{
			name: "ended sprint with open tasks", kind: Sprint, start: "2026-10-01", end: "2026-10-14",
			tasks: []Task{task(1, "done", "2026-10-10"), task(2, "in_progress", "2026-10-25")},
			want:  Status{Status: AtRisk, Total: 2, Done: 1, Reason: "sprint ended on 2026-10-14 with 1 open task"},
		},
		{
			name: "ended sprint all done", kind: Sprint, start: "2026-10-01", end: "2026-10-14",
			tasks: []Task{task(1, "done", "2026-10-10"), task(2, "done", "2026-10-14")},
			want:  Status{Status: OnTrack, Total: 2, Done: 2, Reason: "2 of 2 tasks done, 0 days left"},
		},
		{
			name: "sprint ending today is not ended", kind: Sprint, start: "2026-10-08", end: "2026-10-21",
			tasks: []Task{task(1, "done", "2026-10-21"), task(2, "in_progress", "2026-10-21")},
			want:  Status{Status: AtRisk, Total: 2, Done: 1, Reason: "100% of time elapsed but only 50% of tasks done"},
		},
		{
			name: "task due after sprint end", kind: Sprint, start: "2026-10-15", end: "2026-10-28",
			tasks: []Task{task(1, "not_started", "2026-10-29"), task(2, "done", "2026-11-30")},
			want:  Status{Status: AtRisk, Total: 2, Done: 1, Reason: "1 task due after sprint end"},
		},
		{
			name: "task due on sprint end is fine", kind: Sprint, start: "2026-10-21", end: "2026-10-28",
			tasks: []Task{task(1, "not_started", "2026-10-28")},
			want:  Status{Status: OnTrack, Total: 1, Reason: "0 of 1 tasks done, 8 days left"},
		},
		{
			name: "elapsed minus done exactly 25 is on track", kind: Sprint, start: "2026-10-20", end: "2026-10-23",
			tasks: []Task{task(1, "done", "2026-10-23"), task(2, "in_progress", "2026-10-23"), task(3, "not_started", "2026-10-23"), task(4, "not_started", "2026-10-23")},
			want:  Status{Status: OnTrack, Total: 4, Done: 1, Reason: "1 of 4 tasks done, 3 days left"},
		},
		{
			name: "elapsed minus done over 25 is at risk", kind: Sprint, start: "2026-10-20", end: "2026-10-23",
			tasks: []Task{task(1, "in_progress", "2026-10-23"), task(2, "not_started", "2026-10-23")},
			want:  Status{Status: AtRisk, Total: 2, Reason: "50% of time elapsed but only 0% of tasks done"},
		},
		{
			name: "percentages round down in reason", kind: Sprint, start: "2026-10-19", end: "2026-10-24",
			tasks: []Task{task(1, "done", "2026-10-24"), task(2, "in_progress", "2026-10-24"), task(3, "in_progress", "2026-10-24"),
				task(4, "in_progress", "2026-10-24"), task(5, "in_progress", "2026-10-24"), task(6, "in_progress", "2026-10-24"),
				task(7, "in_progress", "2026-10-24"), task(8, "in_progress", "2026-10-24")},
			want: Status{Status: AtRisk, Total: 8, Done: 1, Reason: "50% of time elapsed but only 12% of tasks done"},
		},
		{
			name: "sprint not started skips elapsed rule", kind: Sprint, start: "2026-10-25", end: "2026-10-30",
			tasks: []Task{task(1, "not_started", "2026-10-28"), task(2, "not_started", "2026-10-30")},
			want:  Status{Status: OnTrack, Total: 2, Reason: "0 of 2 tasks done, 6 days left"},
		},
		{
			name: "one day left", kind: Sprint, start: "2026-10-21", end: "2026-10-21",
			tasks: []Task{task(1, "done", "2026-10-21")},
			want:  Status{Status: OnTrack, Total: 1, Done: 1, Reason: "1 of 1 tasks done, 1 day left"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var start, end time.Time
			if tc.start != "" {
				start, end = d(tc.start), d(tc.end)
			}
			got := Evaluate(tc.kind, tc.tasks, start, end, today)
			if got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestEvaluateIgnoresClockTime(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	lateEvening := time.Date(2026, 10, 21, 23, 59, 0, 0, ist)
	got := Evaluate(Project, []Task{task(1, "not_started", "2026-10-21")}, time.Time{}, time.Time{}, lateEvening)
	if got.Status != OnTrack || got.Overdue != 0 {
		t.Fatalf("got %+v", got)
	}
	got = Evaluate(Project, []Task{task(1, "not_started", "2026-10-20")}, time.Time{}, time.Time{}, time.Date(2026, 10, 21, 0, 1, 0, 0, ist))
	if got.Status != AtRisk || got.Overdue != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 40); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := Truncate(strings.Repeat("日", 41), 40); got != strings.Repeat("日", 39)+"…" {
		t.Fatalf("got %q", got)
	}
}
