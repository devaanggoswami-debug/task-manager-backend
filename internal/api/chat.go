package api

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskmanager/backend/internal/classify"
	"taskmanager/backend/internal/status"
	"taskmanager/backend/internal/store"
)

const (
	maxListedNames = 10

	chatExampleCreate = `create task "Fix login bug" for Alice due tomorrow in sprint "Sprint 1" projects Auth and Billing`
	chatExampleStatus = `what's the status of sprint "Sprint 1"?`

	helpReply = "I can create tasks and report sprint or project status. Try:\n" +
		"- " + chatExampleCreate + "\n" +
		"- " + chatExampleStatus
)

type chatResponse struct {
	Reply   string          `json:"reply"`
	Task    *taskView       `json:"task"`
	Status  *statusResponse `json:"status"`
	Changed bool            `json:"changed"`
}

type chatKind int

const (
	chatHelp chatKind = iota
	chatCreate
	chatStatus
)

// chatCommand is a parsed chat message. create is set for chatCreate and
// scope for chatStatus.
type chatCommand struct {
	kind   chatKind
	create createCommand
	scope  scopeRef
}

// createCommand holds the clauses of a create-task message. Names are as
// typed and still need resolving.
type createCommand struct {
	description string
	assignee    string
	due         time.Time
	hasDue      bool
	sprint      string
	hasSprint   bool
	projects    []string
}

// scopeRef is the sprint or project named in a status query. kind is empty
// when none was named. words are the words following the keyword; a quoted
// name is a single exact word.
type scopeRef struct {
	kind   string
	quoted bool
	words  []string
}

var (
	createIntent = regexp.MustCompile(`(?is)^(?:create|add|new)\s+(?:a\s+)?task\b\s*:?\s*(.*)$`)
	statusIntent = regexp.MustCompile(`(?i)\b(?:status|on track|at risk|blocked|how is|progress)\b|\bhow['’]s\b`)
)

// parseChat classifies msg and extracts its clauses. Relative dates are
// resolved against today.
func parseChat(msg string, today time.Time) chatCommand {
	if m := createIntent.FindStringSubmatch(msg); m != nil {
		return chatCommand{kind: chatCreate, create: parseCreate(m[1], today)}
	}
	if statusIntent.MatchString(msg) {
		return chatCommand{kind: chatStatus, scope: parseScope(tokenize(msg))}
	}
	return chatCommand{kind: chatHelp}
}

// token is a word, a quoted string (text without quotes) or a punctuation
// mark, with its byte range in the source.
type token struct {
	text       string
	lower      string
	quoted     bool
	punct      bool
	start, end int
}

func isQuote(r rune) bool { return r == '"' || r == '“' || r == '”' }

func isPunct(s string, i int) bool {
	r, size := utf8.DecodeRuneInString(s[i:])
	switch r {
	case ',', ';', '?', '!':
		return true
	case '.':
		// A dot ends a clause only at a word boundary, so "v1.2" stays whole.
		if i+size == len(s) {
			return true
		}
		next, _ := utf8.DecodeRuneInString(s[i+size:])
		return unicode.IsSpace(next) || isQuote(next)
	}
	return false
}

func tokenize(s string) []token {
	var toks []token
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r):
			i += size
		case isQuote(r):
			start := i
			i += size
			inner := s[i:]
			if j := strings.IndexFunc(inner, isQuote); j >= 0 {
				inner = inner[:j]
				_, closeSize := utf8.DecodeRuneInString(s[i+j:])
				i += j + closeSize
			} else {
				i = len(s)
			}
			inner = strings.TrimSpace(inner)
			toks = append(toks, token{text: inner, lower: strings.ToLower(inner), quoted: true, start: start, end: i})
		case isPunct(s, i):
			toks = append(toks, token{text: s[i : i+size], punct: true, start: i, end: i + size})
			i += size
		default:
			start := i
			for i < len(s) {
				r, size := utf8.DecodeRuneInString(s[i:])
				if unicode.IsSpace(r) || isQuote(r) || isPunct(s, i) {
					break
				}
				i += size
			}
			w := s[start:i]
			toks = append(toks, token{text: w, lower: strings.ToLower(w), start: start, end: i})
		}
	}
	return toks
}

func isWord(toks []token, i int, words ...string) bool {
	if i >= len(toks) || toks[i].quoted || toks[i].punct {
		return false
	}
	for _, w := range words {
		if toks[i].lower == w {
			return true
		}
	}
	return false
}

// isClauseStart reports whether a create clause begins at toks[i].
func isClauseStart(toks []token, i int) bool {
	return isWord(toks, i, "for", "due", "sprint", "project", "projects") ||
		(isWord(toks, i, "in") && isWord(toks, i+1, "sprint", "project", "projects"))
}

// readName reads a quoted name, or unquoted words up to the next clause,
// punctuation or quote (and, when stopAtAnd, the word "and"). It returns
// the name and the index after it.
func readName(toks []token, i int, stopAtAnd bool) (string, int, bool) {
	if i < len(toks) && toks[i].quoted {
		return toks[i].text, i + 1, toks[i].text != ""
	}
	j := i
	var words []string
	for j < len(toks) && !toks[j].quoted && !toks[j].punct && !isClauseStart(toks, j) && !(stopAtAnd && isWord(toks, j, "and")) {
		words = append(words, toks[j].text)
		j++
	}
	return strings.Join(words, " "), j, len(words) > 0
}

// readNameList reads names separated by "and" or ",".
func readNameList(toks []token, i int) ([]string, int) {
	var names []string
	for {
		name, j, ok := readName(toks, i, true)
		if !ok {
			return names, i
		}
		names = append(names, name)
		i = j
		sep := isWord(toks, i, "and") || (i < len(toks) && toks[i].punct && toks[i].text == ",")
		if !sep {
			return names, i
		}
		if _, _, more := readName(toks, i+1, true); !more {
			return names, i
		}
		i++
	}
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

var smallNumber = regexp.MustCompile(`^[0-9]{1,4}$`)

// parseDue reads a due date at toks[i]: YYYY-MM-DD, today, tomorrow, a
// weekday (next occurrence strictly after today) or "in N days".
func parseDue(toks []token, i int, today time.Time) (time.Time, int, bool) {
	if i >= len(toks) || toks[i].quoted || toks[i].punct {
		return time.Time{}, i, false
	}
	w := toks[i].lower
	if d, ok := parseDate(w); ok {
		return d, i + 1, true
	}
	switch w {
	case "today":
		return today, i + 1, true
	case "tomorrow":
		return today.AddDate(0, 0, 1), i + 1, true
	case "in":
		if i+2 < len(toks) && !toks[i+1].quoted && smallNumber.MatchString(toks[i+1].text) && isWord(toks, i+2, "day", "days") {
			n, _ := strconv.Atoi(toks[i+1].text)
			return today.AddDate(0, 0, n), i + 3, true
		}
		return time.Time{}, i, false
	}
	if wd, ok := weekdays[w]; ok {
		ahead := (int(wd)-int(today.Weekday())+6)%7 + 1
		return today.AddDate(0, 0, ahead), i + 1, true
	}
	return time.Time{}, i, false
}

type clauseSpan struct{ from, to int }

// parseCreate extracts the clauses of a create-task message (the text
// after the leading verb). A repeated clause keeps its last occurrence and
// earlier ones stay in the description.
func parseCreate(rest string, today time.Time) createCommand {
	toks := tokenize(rest)
	var c createCommand
	var assigneeSpan, dueSpan, sprintSpan, projectSpan *clauseSpan

	for i := 0; i < len(toks); {
		from, k := i, i
		if isWord(toks, i, "in") && isWord(toks, i+1, "sprint", "project", "projects") {
			k = i + 1
		}
		switch {
		case isWord(toks, k, "for"):
			if name, j, ok := readName(toks, k+1, false); ok {
				c.assignee, assigneeSpan = name, &clauseSpan{from, j}
				i = j
				continue
			}
		case isWord(toks, k, "due"):
			if d, j, ok := parseDue(toks, k+1, today); ok {
				c.due, c.hasDue, dueSpan = d, true, &clauseSpan{from, j}
				i = j
				continue
			}
		case isWord(toks, k, "sprint"):
			if name, j, ok := readName(toks, k+1, false); ok {
				c.sprint, c.hasSprint, sprintSpan = name, true, &clauseSpan{from, j}
				i = j
				continue
			}
		case isWord(toks, k, "project", "projects"):
			if names, j := readNameList(toks, k+1); len(names) > 0 {
				c.projects, projectSpan = names, &clauseSpan{from, j}
				i = j
				continue
			}
		}
		i++
	}

	consumed := make([]bool, len(toks))
	for _, sp := range []*clauseSpan{assigneeSpan, dueSpan, sprintSpan, projectSpan} {
		if sp == nil {
			continue
		}
		for i := sp.from; i < sp.to; i++ {
			consumed[i] = true
		}
	}
	c.description = remainingDescription(toks, consumed, rest)
	return c
}

var spaceBeforePunct = regexp.MustCompile(`\s+([,;.?!])`)

// remainingDescription returns the first unconsumed quoted string, or else
// the unconsumed text trimmed of separators.
func remainingDescription(toks []token, consumed []bool, src string) string {
	for i, t := range toks {
		if !consumed[i] && t.quoted && t.text != "" {
			return t.text
		}
	}
	var b strings.Builder
	for i, t := range toks {
		if consumed[i] || t.quoted {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(src[t.start:t.end])
	}
	out := spaceBeforePunct.ReplaceAllString(b.String(), "$1")
	return strings.Trim(out, " \t\n,;.:-–—")
}

// parseScope finds the first "sprint <name>" or "project <name>" in toks.
func parseScope(toks []token) scopeRef {
	for i := range toks {
		if !isWord(toks, i, "sprint", "project") {
			continue
		}
		ref := scopeRef{kind: toks[i].lower}
		if i+1 < len(toks) && toks[i+1].quoted {
			ref.quoted, ref.words = true, []string{toks[i+1].text}
			return ref
		}
		for j := i + 1; j < len(toks) && !toks[j].quoted && !toks[j].punct; j++ {
			ref.words = append(ref.words, toks[j].text)
		}
		return ref
	}
	return scopeRef{}
}

// named is a sprint or project a typed name can resolve to.
type named struct {
	id   int64
	name string
}

// resolveName matches name case-insensitively: a unique exact match first,
// then a unique prefix. On failure it returns the ambiguous candidates, if
// any.
func resolveName(name string, cands []named) (named, []named, bool) {
	if name == "" {
		return named{}, nil, false
	}
	var exact, prefixed []named
	lower := strings.ToLower(name)
	for _, c := range cands {
		if strings.EqualFold(c.name, name) {
			exact = append(exact, c)
		}
		if strings.HasPrefix(strings.ToLower(c.name), lower) {
			prefixed = append(prefixed, c)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil, true
	case len(exact) > 1:
		return named{}, exact, false
	case len(prefixed) == 1:
		return prefixed[0], nil, true
	}
	return named{}, prefixed, false
}

// resolveScope resolves the longest leading run of ref.words that names a
// candidate, so "S1 on track" resolves to S1.
func resolveScope(ref scopeRef, cands []named) (named, []named, bool) {
	var ambiguous []named
	for k := len(ref.words); k >= 1; k-- {
		m, amb, ok := resolveName(strings.Join(ref.words[:k], " "), cands)
		if ok {
			return m, nil, true
		}
		if ambiguous == nil {
			ambiguous = amb
		}
	}
	return named{}, ambiguous, false
}

func nameList(cands []named) string {
	names := make([]string, 0, maxListedNames)
	for i, c := range cands {
		if i == maxListedNames {
			names = append(names, "…")
			break
		}
		names = append(names, c.name)
	}
	return strings.Join(names, ", ")
}

func nameProblem(kind, name string, ambiguous []named) string {
	if len(ambiguous) > 0 {
		return kind + ` "` + name + `" is ambiguous (` + nameList(ambiguous) + ")"
	}
	return `no ` + kind + ` matches "` + name + `"`
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func sprintNames(sprints []store.Sprint) []named {
	out := make([]named, len(sprints))
	for i, s := range sprints {
		out[i] = named{s.ID, s.Name}
	}
	return out
}

// labelSprints renames sprint candidates for listing, as "Sprint 1 (Auth)"
// or "Sprint 1 (unassigned)", so sprints sharing a name can be told apart.
func labelSprints(cands []named, sprints []store.Sprint, projects []store.Project) []named {
	projectName := map[int64]string{}
	for _, p := range projects {
		projectName[p.ID] = p.Name
	}
	owner := map[int64]string{}
	for _, s := range sprints {
		owner[s.ID] = "unassigned"
		if s.ProjectID != nil {
			owner[s.ID] = projectName[*s.ProjectID]
		}
	}
	out := make([]named, len(cands))
	for i, c := range cands {
		out[i] = named{c.id, c.name + " (" + owner[c.id] + ")"}
	}
	return out
}

// resolveSprint resolves a sprint name among the sprints of projectIDs
// first, when any are given, and otherwise (or when nothing there matches)
// among all sprints. Ambiguous candidates come back labelled with their
// project.
func resolveSprint(name string, sprints []store.Sprint, projects []store.Project, projectIDs []int64) (named, []named, bool) {
	cands := sprintNames(sprints)
	m, amb, ok := named{}, []named(nil), false
	if len(projectIDs) > 0 {
		var scoped []named
		for i, s := range sprints {
			if s.ProjectID != nil && slices.Contains(projectIDs, *s.ProjectID) {
				scoped = append(scoped, cands[i])
			}
		}
		m, amb, ok = resolveName(name, scoped)
	}
	if !ok && len(amb) == 0 {
		m, amb, ok = resolveName(name, cands)
	}
	return m, labelSprints(amb, sprints, projects), ok
}

func projectNames(projects []store.Project) []named {
	out := make([]named, len(projects))
	for i, p := range projects {
		out[i] = named{p.ID, p.Name}
	}
	return out
}

func (h *handler) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if !decodeJSON(w, r, &req, "request body must be a single valid JSON object with only message") {
		return
	}
	msg, fields := req.validate()
	if fields != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: fields})
		return
	}

	cmd := parseChat(msg, h.today())
	resp := chatResponse{Reply: helpReply}
	var err error
	switch cmd.kind {
	case chatCreate:
		resp, err = h.chatCreate(r.Context(), cmd.create)
	case chatStatus:
		resp, err = h.chatStatus(r.Context(), cmd.scope)
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) chatCreate(ctx context.Context, c createCommand) (chatResponse, error) {
	var missing []string
	if c.description == "" {
		missing = append(missing, "a description")
	}
	if c.assignee == "" {
		missing = append(missing, "an assignee (for <name>)")
	}
	if !c.hasDue {
		missing = append(missing, "a due date (due YYYY-MM-DD, today, tomorrow, a weekday or in N days)")
	}
	if len(missing) > 0 {
		return chatResponse{Reply: "I couldn't create the task: it needs " + strings.Join(missing, ", ") +
			".\nExample: " + chatExampleCreate}, nil
	}
	if msg := checkText(c.assignee, maxAssigneeLen); msg != "" {
		return chatResponse{Reply: "I couldn't create the task: the assignee " + msg + "."}, nil
	}
	if msg := checkText(c.description, maxDescriptionLen); msg != "" {
		return chatResponse{Reply: "I couldn't create the task: the description " + msg + "."}, nil
	}

	var projects []store.Project
	if c.hasSprint || len(c.projects) > 0 {
		var err error
		if projects, err = h.store.ListProjects(ctx); err != nil {
			return chatResponse{}, err
		}
	}

	var problems, projectProblems []string
	sprintIDs, projectIDs := []int64{}, []int64{}
	var sprintName string
	var projectNamesOut []string
	if len(c.projects) > 0 {
		cands := projectNames(projects)
		seen := map[int64]bool{}
		for _, name := range c.projects {
			m, amb, ok := resolveName(name, cands)
			switch {
			case !ok:
				projectProblems = append(projectProblems, nameProblem("project", name, amb))
			case !seen[m.id]:
				seen[m.id] = true
				projectIDs = append(projectIDs, m.id)
				projectNamesOut = append(projectNamesOut, m.name)
			}
		}
	}
	if c.hasSprint {
		sprints, err := h.store.ListSprints(ctx)
		if err != nil {
			return chatResponse{}, err
		}
		m, amb, ok := resolveSprint(c.sprint, sprints, projects, projectIDs)
		if !ok {
			problems = append(problems, nameProblem("sprint", c.sprint, amb))
		} else {
			sprintIDs, sprintName = []int64{m.id}, m.name
			// The task joins the sprint's project too.
			for _, s := range sprints {
				if s.ID == m.id && s.ProjectID != nil && !slices.Contains(projectIDs, *s.ProjectID) {
					projectIDs = append(projectIDs, *s.ProjectID)
					for _, p := range projects {
						if p.ID == *s.ProjectID {
							projectNamesOut = append(projectNamesOut, p.Name)
						}
					}
				}
			}
		}
	}
	problems = append(problems, projectProblems...)
	if len(problems) > 0 {
		return chatResponse{Reply: "I couldn't create the task: " + strings.Join(problems, "; ") + "."}, nil
	}

	category, defaulted := classify.Classify(c.description)
	t, err := h.store.Create(ctx, store.TaskInput{
		Assignee: c.assignee, DueDate: c.due, Description: c.description, Status: defaultStatus,
		Category: &category, SprintIDs: sprintIDs, ProjectIDs: projectIDs,
	})
	if err != nil {
		return chatResponse{}, err
	}

	var b strings.Builder
	b.WriteString("Created task #" + strconv.FormatInt(t.ID, 10) + ` "` + t.Description + `" for ` + t.Assignee +
		", due " + t.DueDate.Format(dateLayout) + ", category " + capitalize(category))
	if defaulted {
		b.WriteString(" (default — no keywords matched)")
	}
	if sprintName != "" {
		b.WriteString(", sprint " + sprintName)
	}
	if len(projectNamesOut) > 0 {
		b.WriteString(", projects " + strings.Join(projectNamesOut, ", "))
	}
	b.WriteString(".")
	v := toView(t)
	return chatResponse{Reply: b.String(), Task: &v, Changed: true}, nil
}

var statusLabels = map[string]string{status.OnTrack: "ON TRACK", status.AtRisk: "AT RISK", status.Blocked: "BLOCKED"}

func (h *handler) chatStatus(ctx context.Context, ref scopeRef) (chatResponse, error) {
	sprints, err := h.store.ListSprints(ctx)
	if err != nil {
		return chatResponse{}, err
	}
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return chatResponse{}, err
	}

	var problem string
	if ref.kind != "" && len(ref.words) > 0 {
		cands := projectNames(projects)
		if ref.kind == "sprint" {
			cands = sprintNames(sprints)
		}
		m, amb, ok := resolveScope(ref, cands)
		if ok {
			return h.scopeStatusReply(ctx, ref.kind, m, sprints)
		}
		if ref.kind == "sprint" {
			amb = labelSprints(amb, sprints, projects)
		}
		problem = capitalize(nameProblem(ref.kind, strings.Join(ref.words, " "), amb)) + ". "
	}

	reply := problem + "Which sprint or project do you mean?"
	if len(sprints) == 0 && len(projects) == 0 {
		reply += " There are no sprints or projects yet."
	}
	if len(sprints) > 0 {
		reply += " Sprints: " + nameList(sprintNames(sprints)) + "."
	}
	if len(projects) > 0 {
		reply += " Projects: " + nameList(projectNames(projects)) + "."
	}
	return chatResponse{Reply: reply}, nil
}

func (h *handler) scopeStatusReply(ctx context.Context, kind string, m named, sprints []store.Sprint) (chatResponse, error) {
	var st status.Status
	var err error
	if kind == "sprint" {
		for _, s := range sprints {
			if s.ID == m.id {
				st, err = h.evaluateSprint(ctx, s)
				break
			}
		}
	} else {
		st, err = h.evaluateProject(ctx, store.Project{ID: m.id, Name: m.name})
	}
	if err != nil {
		return chatResponse{}, err
	}
	sr := toStatusResponse(st)
	reply := capitalize(kind) + ` "` + m.name + `" is ` + statusLabels[st.Status] + ": " + st.Reason + "."
	return chatResponse{Reply: reply, Status: &sr}, nil
}
