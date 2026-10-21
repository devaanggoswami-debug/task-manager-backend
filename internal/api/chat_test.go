package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// chatToday is a Wednesday.
var chatToday = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

func day(s string) time.Time {
	d, ok := parseDate(s)
	if !ok {
		panic(s)
	}
	return d
}

func TestParseCreate(t *testing.T) {
	tests := []struct {
		msg  string
		want createCommand
	}{
		{
			`create task "Fix login bug" for Alice due tomorrow in sprint "S1" projects Auth and Billing`,
			createCommand{description: "Fix login bug", assignee: "Alice", due: day("2026-10-22"), hasDue: true,
				sprint: "S1", hasSprint: true, projects: []string{"Auth", "Billing"}},
		},
		{
			`Add a task: Write release notes for Bob due 2026-11-03`,
			createCommand{description: "Write release notes", assignee: "Bob", due: day("2026-11-03"), hasDue: true},
		},
		{
			`new task Prepare budget for Carol Ann, due friday, project "Billing"`,
			createCommand{description: "Prepare budget", assignee: "Carol Ann", due: day("2026-10-23"), hasDue: true, projects: []string{"Billing"}},
		},
		{
			`create task review for Dan due wednesday`,
			createCommand{description: "review", assignee: "Dan", due: day("2026-10-28"), hasDue: true},
		},
		{
			`create task review for Dan due Tuesday`,
			createCommand{description: "review", assignee: "Dan", due: day("2026-10-27"), hasDue: true},
		},
		{
			`create task: ship it due in 3 days for Eve sprint S2`,
			createCommand{description: "ship it", assignee: "Eve", due: day("2026-10-24"), hasDue: true, sprint: "S2", hasSprint: true},
		},
		{
			`create task ship it due in 1 day for Eve`,
			createCommand{description: "ship it", assignee: "Eve", due: day("2026-10-22"), hasDue: true},
		},
		{
			`CREATE TASK "Audit logs" FOR alice DUE TODAY IN PROJECT auth`,
			createCommand{description: "Audit logs", assignee: "alice", due: chatToday, hasDue: true, projects: []string{"auth"}},
		},
		{
			`create task “Curly quoted” for Al due today projects "Mobile App", Billing and "Auth"`,
			createCommand{description: "Curly quoted", assignee: "Al", due: chatToday, hasDue: true, projects: []string{"Mobile App", "Billing", "Auth"}},
		},
		{
			`create task build dashboard for marketing for Alice due today`,
			createCommand{description: "build dashboard for marketing", assignee: "Alice", due: chatToday, hasDue: true},
		},
		{
			`create task investigate due to outage for Bob due today.`,
			createCommand{description: "investigate due to outage", assignee: "Bob", due: chatToday, hasDue: true},
		},
		{
			`create task plan sprint for Alice due today`,
			createCommand{description: "plan sprint", assignee: "Alice", due: chatToday, hasDue: true},
		},
		{
			`create task for Alice due 2026-02-30 "Bad date"`,
			createCommand{description: "Bad date", assignee: "Alice"},
		},
		{
			`create task fix the bug, then deploy v1.2 for Zed due today`,
			createCommand{description: "fix the bug, then deploy v1.2", assignee: "Zed", due: chatToday, hasDue: true},
		},
		{`create task`, createCommand{}},
		{`add task fix bug`, createCommand{description: "fix bug"}},
	}
	for _, tc := range tests {
		t.Run(tc.msg, func(t *testing.T) {
			cmd := parseChat(tc.msg, chatToday)
			if cmd.kind != chatCreate {
				t.Fatalf("kind = %v", cmd.kind)
			}
			if !reflect.DeepEqual(cmd.create, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", cmd.create, tc.want)
			}
		})
	}
}

func TestParseIntent(t *testing.T) {
	tests := []struct {
		msg   string
		kind  chatKind
		scope scopeRef
	}{
		{"create tasks for Alice", chatHelp, scopeRef{}},
		{"create a new task", chatHelp, scopeRef{}},
		{"hello", chatHelp, scopeRef{}},
		{"show sprint S1", chatHelp, scopeRef{}},
		{`for Bob due tomorrow create task x`, chatHelp, scopeRef{}},
		{"create task status report for A due today", chatCreate, scopeRef{}},
		{`what's the status of sprint "S1"?`, chatStatus, scopeRef{kind: "sprint", quoted: true, words: []string{"S1"}}},
		{"how is project Billing doing?", chatStatus, scopeRef{kind: "project", words: []string{"Billing", "doing"}}},
		{"how’s Sprint s1", chatStatus, scopeRef{kind: "sprint", words: []string{"s1"}}},
		{"is sprint S1 on track", chatStatus, scopeRef{kind: "sprint", words: []string{"S1", "on", "track"}}},
		{"anything blocked in project Auth?", chatStatus, scopeRef{kind: "project", words: []string{"Auth"}}},
		{"at risk items for sprint", chatStatus, scopeRef{kind: "sprint"}},
		{"progress?", chatStatus, scopeRef{}},
	}
	for _, tc := range tests {
		t.Run(tc.msg, func(t *testing.T) {
			cmd := parseChat(tc.msg, chatToday)
			if cmd.kind != tc.kind || !reflect.DeepEqual(cmd.scope, tc.scope) {
				t.Fatalf("got kind %v scope %+v", cmd.kind, cmd.scope)
			}
		})
	}
}

func TestResolveName(t *testing.T) {
	cands := []named{{1, "S1"}, {2, "S10"}, {3, "Billing"}, {4, "Mobile App"}}
	tests := []struct {
		name    string
		wantID  int64
		wantAmb []string
	}{
		{"s1", 1, nil},
		{"S10", 2, nil},
		{"bill", 3, nil},
		{"mobile", 4, nil},
		{"MOBILE APP", 4, nil},
		{"S", 0, []string{"S1", "S10"}},
		{"nope", 0, nil},
		{"", 0, nil},
	}
	for _, tc := range tests {
		m, amb, ok := resolveName(tc.name, cands)
		var ambNames []string
		for _, a := range amb {
			ambNames = append(ambNames, a.name)
		}
		if ok != (tc.wantID != 0) || m.id != tc.wantID || !reflect.DeepEqual(ambNames, tc.wantAmb) {
			t.Fatalf("%q: got %+v %v %v", tc.name, m, ambNames, ok)
		}
	}

	if m, _, ok := resolveScope(scopeRef{kind: "sprint", words: []string{"S1", "on", "track"}}, cands); !ok || m.id != 1 {
		t.Fatalf("longest prefix: %+v %v", m, ok)
	}
	if m, _, ok := resolveScope(scopeRef{kind: "project", words: []string{"Mobile", "App", "doing"}}, cands); !ok || m.id != 4 {
		t.Fatalf("multi-word: %+v %v", m, ok)
	}
	if _, amb, ok := resolveScope(scopeRef{kind: "sprint", words: []string{"S", "going"}}, cands); ok || len(amb) != 2 {
		t.Fatalf("ambiguous: %v %v", amb, ok)
	}
}

func chat(t *testing.T, h http.Handler, msg string) chatResponse {
	t.Helper()
	rec := do(t, h, "POST", "/api/chat", `{"message":`+jsonString(msg)+`}`)
	expectStatus(t, rec, http.StatusOK)
	return decode[chatResponse](t, rec)
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestChatCreate(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seed(t, h, fs)

	got := chat(t, h, `create task "Fix login bug" for Alice due tomorrow in sprint "S1" projects Auth and Billing`)
	wantReply := `Created task #1 "Fix login bug" for Alice, due 2026-10-22, category Engineering, sprint S1, projects Auth, Billing.`
	if got.Reply != wantReply || !got.Changed || got.Task == nil || got.Status != nil {
		t.Fatalf("got %+v", got)
	}
	if *got.Task.Category != "engineering" || got.Task.Status != "not_started" ||
		!reflect.DeepEqual(got.Task.SprintIDs, []int64{1}) || !reflect.DeepEqual(got.Task.ProjectIDs, []int64{1, 2}) {
		t.Fatalf("task = %+v", got.Task)
	}

	got = chat(t, h, `add task: organise the offsite for Bob due friday sprint s2 project bill`)
	wantReply = `Created task #2 "organise the offsite" for Bob, due 2026-10-23, category Engineering (default — no keywords matched), sprint S2, projects Billing.`
	if got.Reply != wantReply || !got.Changed {
		t.Fatalf("got %+v", got)
	}

	got = chat(t, h, `new task "Send invoices" for Carol due in 5 days`)
	if got.Reply != `Created task #3 "Send invoices" for Carol, due 2026-10-26, category Finance.` {
		t.Fatalf("got %q", got.Reply)
	}
	if len(got.Task.SprintIDs) != 0 || len(got.Task.ProjectIDs) != 0 {
		t.Fatalf("task = %+v", got.Task)
	}

	noCreate := []struct{ msg, contains string }{
		{`create task`, "it needs a description, an assignee (for <name>), a due date"},
		{`create task fix bug for Alice`, "it needs a due date"},
		{`create task for Alice due today`, "it needs a description."},
		{`create task "x" for Alice due today sprint S9`, `no sprint matches "S9"`},
		{`create task "x" for Alice due today sprint S`, `sprint "S" is ambiguous (S1 (unassigned), S2 (unassigned))`},
		{`create task "x" for Alice due today projects Auth and Nope`, `no project matches "Nope"`},
		{`create task "x" for ` + strings.Repeat("a", 101) + ` due today`, "the assignee must be at most 100 characters"},
	}
	for _, tc := range noCreate {
		t.Run(tc.msg, func(t *testing.T) {
			got := chat(t, h, tc.msg)
			if got.Changed || got.Task != nil || !strings.Contains(got.Reply, tc.contains) {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if list := decode[[]taskView](t, do(t, h, "GET", "/api/tasks", "")); len(list) != 3 {
		t.Fatalf("failed chats created tasks: %d", len(list))
	}
}

func TestChatStatus(t *testing.T) {
	fs := newFakeStore()
	h := newClockHandler(fs)
	seed(t, h, fs)
	chat(t, h, `create task "Fix login bug" for Alice due 2026-10-27 in sprint S1 project Auth`)
	chat(t, h, `create task "Plan pricing" for Bob due 2026-10-27 in sprint S1 projects Auth, Billing`)
	expectStatus(t, do(t, h, "PUT", "/api/tasks/2", `{"assignee":"Bob","date":"2026-10-27","description":"Plan pricing","status":"blocked"}`), http.StatusOK)

	got := chat(t, h, `what's the status of sprint "S1"?`)
	if got.Reply != `Sprint "S1" is BLOCKED: 1 blocked task: #2 "Plan pricing".` || got.Changed || got.Task != nil ||
		got.Status == nil || *got.Status != (statusResponse{Status: "blocked", Reason: `1 blocked task: #2 "Plan pricing"`, Total: 2, Blocked: 1}) {
		t.Fatalf("got %+v", got)
	}

	expectStatus(t, do(t, h, "PUT", "/api/tasks/2", `{"assignee":"Bob","date":"2026-10-27","description":"Plan pricing","status":"done"}`), http.StatusOK)
	got = chat(t, h, "how is project bill doing?")
	if got.Reply != `Project "Billing" is ON TRACK: 1 of 1 tasks done.` || got.Status.Status != "on_track" {
		t.Fatalf("got %+v", got)
	}
	got = chat(t, h, "is sprint s1 on track")
	if got.Reply != `Sprint "S1" is ON TRACK: 1 of 2 tasks done, 8 days left.` {
		t.Fatalf("got %+v", got)
	}

	ask := []struct{ msg, want string }{
		{"status of sprint S9", `No sprint matches "S9". Which sprint or project do you mean? Sprints: S1, S2. Projects: Auth, Billing.`},
		{"status of sprint S", `Sprint "S" is ambiguous (S1 (unassigned), S2 (unassigned)). Which sprint or project do you mean? Sprints: S1, S2. Projects: Auth, Billing.`},
		{"any progress?", `Which sprint or project do you mean? Sprints: S1, S2. Projects: Auth, Billing.`},
		{"status of the sprint", `Which sprint or project do you mean? Sprints: S1, S2. Projects: Auth, Billing.`},
	}
	for _, tc := range ask {
		got := chat(t, h, tc.msg)
		if got.Reply != tc.want || got.Status != nil || got.Changed {
			t.Fatalf("%q: got %+v", tc.msg, got)
		}
	}

	empty := newClockHandler(newFakeStore())
	if got := chat(t, empty, "status?"); got.Reply != "Which sprint or project do you mean? There are no sprints or projects yet." {
		t.Fatalf("got %q", got.Reply)
	}
}

func TestChatHelpAndValidation(t *testing.T) {
	h := newClockHandler(newFakeStore())
	rec := do(t, h, "POST", "/api/chat", `{"message":"  hello  "}`)
	expectStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"task":null,"status":null,"changed":false`) {
		t.Fatalf("body = %s", body)
	}
	if got := decode[chatResponse](t, rec); got.Reply != helpReply || !strings.Contains(got.Reply, "create task") || !strings.Contains(got.Reply, "status of sprint") {
		t.Fatalf("got %+v", got)
	}

	for body, msg := range map[string]string{
		`{}`:                    "is required",
		`{"message":"   "}`:     "is required",
		`{"message":"a\u0000"}`: "must not contain NUL characters",
		`{"message":"` + strings.Repeat("é", 501) + `"}`: "must be at most 500 characters",
	} {
		rec := do(t, h, "POST", "/api/chat", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields["message"] != msg || len(got.Fields) != 1 {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	expectStatus(t, do(t, h, "POST", "/api/chat", `{"message":"`+strings.Repeat("é", 500)+`"}`), http.StatusOK)
	for _, body := range []string{`{"message":"hi","x":1}`, `{"message":1}`, `["hi"]`, `{"message":"hi"}{}`} {
		rec := do(t, h, "POST", "/api/chat", body)
		expectStatus(t, rec, http.StatusBadRequest)
		if got := decode[errorResponse](t, rec); got.Fields != nil {
			t.Fatalf("%s: %+v", body, got)
		}
	}
	expectStatus(t, do(t, h, "POST", "/api/chat", `{"message":"`+strings.Repeat("x", maxBodyBytes)+`"}`), http.StatusRequestEntityTooLarge)
}
