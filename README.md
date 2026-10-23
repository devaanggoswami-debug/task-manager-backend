# Task Manager Backend

A Go HTTP REST API for a project and task management application. It persists
tasks, comments, projects, sprints, and their relationships in PostgreSQL, and
exposes a chat endpoint that handles task creation and status queries
deterministically while delegating free-form questions to Google's Gemini when
a key is configured. The web client is maintained separately in
[task-manager-frontend](https://github.com/devaanggoswami-debug/task-manager-frontend).

- **Default API base URL (local):** `http://127.0.0.1:8080`
- **Database:** PostgreSQL, default database name `taskmanager`
- **Repository:** [task-manager-backend](https://github.com/devaanggoswami-debug/task-manager-backend)
- **Schema:** [`db/schema.sql`](db/schema.sql)

> **No authentication.** This API has no authentication or authorization of any
> kind — every endpoint is open to anyone who can reach it. By default it binds
> to `127.0.0.1:8080` and only permits the single configured browser origin.
> The server logs a warning when `HTTP_ADDR` is set to a non-loopback address.
> If you deploy it to a public host (for example the live Render service), treat
> all data as world-readable and world-writable: do not store sensitive or
> production data behind it until authentication, authorization, and the other
> production controls have been added.

## Architecture

```
Browser ──HTTP/JSON──▶ Go API (net/http)  ──pgx──▶ PostgreSQL
(Next.js frontend)     │
                       └──HTTPS──▶ Gemini generateContent API (free-form chat only)
```

- **Language/runtime:** Go `1.23+` (module `taskmanager/backend`).
- **HTTP:** the standard library `net/http` router (`http.ServeMux` with
  method-and-path patterns). No web framework.
- **Database:** PostgreSQL via [`pgx/v5`](https://github.com/jackc/pgx) and
  `pgxpool`. The only direct dependency is `github.com/jackc/pgx/v5`.
- **Assistant:** a small, dependency-free Gemini client
  (`internal/gemini`) built on `net/http`; used only for free-form chat
  questions.
- The server owns all validation, business rules, keyword classification,
  project/sprint health calculation, and persistence. The frontend holds no
  business logic.

## Capabilities

Confirmed from the registered routes and handlers:

- **Tasks** — create, read, update, delete; list with search, filters, and
  pagination. Each task carries an assignee, due date, description, status,
  optional category, and project/sprint memberships.
- **Task comments/updates** — list (paginated) and create; comments are deleted
  with their task (`ON DELETE CASCADE`).
- **Projects** — create, list/search, detail with counts, and rename.
- **Sprints** — create, list (with project / unassigned filters), detail,
  update, add tasks, and remove a task. A sprint belongs to a project; a task
  can belong to several projects and sprints.
- **Derived health** — `on_track`, `at_risk`, or `blocked` for any project or
  sprint, with a human-readable reason and task counts.
- **Keyword classification** — a task with no category is classified into one of
  `engineering`, `marketing`, `finance`, `sales`, or `compliance` from its
  description; the default when nothing matches is `engineering`.
- **Chat assistant** (`POST /api/chat`) — deterministic task creation and status
  queries, with free-form questions answered by Gemini when configured.
- Cross-cutting: strict JSON decoding (unknown fields rejected), JSON error
  bodies, single-origin CORS, a 1 MiB request-body limit, request timeouts, and
  graceful shutdown.

## Prerequisites

- **Go 1.23 or newer.**
- **PostgreSQL** (any currently supported server; tested against 16/18). The
  command-line tools `psql`, `createdb`, and `pg_isready` are used by the setup
  steps below.

On this Windows machine, PostgreSQL's command-line tools may live at
`C:\Program Files\PostgreSQL\18\bin`; add that directory to `PATH` if the tools
are not already found.

## Local setup (Windows with Git Bash)

### 1. Clone the repository

```bash
git clone https://github.com/devaanggoswami-debug/task-manager-backend.git
cd task-manager-backend
```

### 2. Configure the local PostgreSQL connection

For a typical local installation, set the connection environment in Git Bash.
The password is read interactively so it never lands in a file or in shell
history:

```bash
export PGHOST=127.0.0.1
export PGPORT=5432
export PGUSER=postgres
export PATH="$PATH:/c/Program Files/PostgreSQL/18/bin"
export DATABASE_URL="postgres://127.0.0.1:5432/taskmanager?sslmode=disable"
read -rsp "PostgreSQL password: " PGPASSWORD
echo
export PGPASSWORD
```

If the PostgreSQL service is stopped, start it from an elevated **PowerShell**
(adjust the version suffix to your install):

```powershell
Start-Service -Name postgresql-x64-18
```

Verify the server is accepting connections:

```bash
pg_isready -h 127.0.0.1 -p 5432
```

### 3. Create the database and apply the schema

These commands create the database only if it does not already exist, then apply
the schema script. [`db/schema.sql`](db/schema.sql) is **idempotent** — it uses
`CREATE TABLE IF NOT EXISTS`, guarded `ALTER`/constraint blocks, and
`CREATE INDEX IF NOT EXISTS`, so it is safe to run on every deploy and also
migrates older databases forward (for example, it converts a legacy `done`
boolean column to the `status` column).

```bash
if ! psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d postgres -Atqc \
  "SELECT 1 FROM pg_database WHERE datname = 'taskmanager'" | grep -q 1; then
  createdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" taskmanager
fi

psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" \
  -d taskmanager -v ON_ERROR_STOP=1 -f db/schema.sql
```

The schema defines:

| Table | Purpose |
|---|---|
| `tasks` | task details, `status`, and nullable `category` |
| `task_comments` | task updates/comments (cascade-deleted with the task) |
| `projects` | projects (unique name, case-insensitive) |
| `sprints` | sprints, each optionally owned by a project, with `start_date`/`end_date` |
| `project_tasks` | project ↔ task memberships |
| `sprint_tasks` | sprint ↔ task memberships |

Status values are `not_started`, `in_progress`, `blocked`, and `done`.
Categories are `engineering`, `marketing`, `finance`, `sales`, and `compliance`.
Sprint names are unique within a project (case-insensitively).

### 4. Run the API

```bash
go run .
```

The API listens on **http://127.0.0.1:8080** by default. A quick check:

```bash
curl -i http://127.0.0.1:8080/api/tasks
```

The frontend expects the API at `http://127.0.0.1:8080` and runs on
`http://127.0.0.1:3100`, which is the default allowed CORS origin.

## Configuration

All settings are read from environment variables at startup
(see `main.go`). A copy-ready template lives in the reference project's
`.env.example`; the values below are placeholders — never commit a real key,
password, or private connection string.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgres://localhost:5432/taskmanager?sslmode=disable` | PostgreSQL connection string (host, port, database, options). |
| `PGUSER` | connection default | Database role, used by `pgx` when not embedded in `DATABASE_URL`. |
| `PGPASSWORD` | unset | Database password; set in the process environment (or `~/.pgpass`), never in source control. |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen address. A non-loopback value logs a warning because the API is unauthenticated. |
| `CORS_ORIGIN` | `http://127.0.0.1:3100` | The single browser origin allowed to call the API. |
| `GEMINI_API_KEY` | unset | Enables the free-form chat assistant. Server-side only; read from the environment and never logged or returned in an error. Leave unset to disable Gemini (the chat endpoint then returns a static help reply for free-form messages). |
| `GEMINI_MODEL` | `gemini-3.8-flash` (code default) | Gemini model used for free-form chat. See [Gemini model selection](#gemini-model-selection). |
| `TEST_DATABASE_URL` | unset | **Test only.** When set, enables the PostgreSQL integration tests against that database; unset skips them. Point it at a throwaway test database, never your real one. |

`DATABASE_URL` controls the host, port, database, and connection options. The
built-in default uses `localhost`; set it explicitly to
`postgres://127.0.0.1:5432/taskmanager?sslmode=disable` for an IPv4 connection
if `localhost` resolves to IPv6 on your machine. When credentials are not
embedded in the URL, `pgx` falls back to the `PGUSER`/`PGPASSWORD` environment
settings. Avoid putting credentials in a committed URL or in shell history.

> **Never** place `GEMINI_API_KEY` (or any secret) in the frontend, in a
> `NEXT_PUBLIC_*` variable, or in client-side code. It belongs only in this
> backend's environment.

## HTTP API reference

All endpoints are rooted at `/api`. Requests and responses are JSON unless the
response is `204 No Content`. Request bodies must be a single JSON object with
`Content-Type: application/json`, at most 1 MiB, and no unknown fields.
Validation failures return `400` with a body shaped like
`{ "error": "validation failed", "fields": { "field": "message" } }`.

### Tasks

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/tasks` | List tasks; supports filters and pagination |
| `POST` | `/api/tasks` | Create a task |
| `GET` | `/api/tasks/{id}` | Get one task |
| `PUT` | `/api/tasks/{id}` | Replace/update task fields |
| `DELETE` | `/api/tasks/{id}` | Delete a task (and its comments) |
| `GET` | `/api/tasks/{id}/comments` | List task updates/comments (paginated) |
| `POST` | `/api/tasks/{id}/comments` | Add a comment/update |

Task list query parameters: `q` (case-insensitive substring of assignee or
description, max 100 chars), `projectId`, `sprintId`, `excludeSprintId`,
`status`, `category`, `page`, and `pageSize`. Valid `status` values:
`not_started`, `in_progress`, `blocked`, `done`. Valid `category` filter values:
`engineering`, `marketing`, `finance`, `sales`, `compliance`, plus
`unclassified` for tasks with no category. Pagination is 1-based; when `page` or
`pageSize` is present it defaults to page 1 / 20 items, and `pageSize` may not
exceed 100. Paged list responses include an `X-Total-Count` header (exposed to
the configured CORS origin).

Example task creation body:

```json
{
  "assignee": "Aditi Rao",
  "date": "2026-10-23",
  "description": "Audit the customer portal workflow",
  "status": "in_progress",
  "category": "engineering",
  "projectIds": [1],
  "sprintIds": [1]
}
```

Required fields: `assignee`, `date` (`YYYY-MM-DD`), and `description`. On create,
`status` defaults to `not_started` when omitted; on update (`PUT`), `status` is
required. If `category` is omitted on create, the backend classifies the
description with keyword rules.

### Projects

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/projects` | List/search projects |
| `POST` | `/api/projects` | Create a project (`{"name":"Customer Portal"}`) |
| `GET` | `/api/projects/{id}` | Get project details and counts |
| `PUT` | `/api/projects/{id}` | Rename a project |
| `GET` | `/api/projects/{id}/status` | Derived project health |

Project list supports `q`, `page`, and `pageSize`. A duplicate name returns
`409`.

### Sprints

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/sprints` | List sprints |
| `POST` | `/api/sprints` | Create a sprint |
| `GET` | `/api/sprints/{id}` | Get sprint details |
| `PUT` | `/api/sprints/{id}` | Update a sprint |
| `GET` | `/api/sprints/{id}/status` | Derived sprint health |
| `POST` | `/api/sprints/{id}/tasks` | Add tasks to a sprint (`{"taskIds":[1,2]}`, 1–100 ids) |
| `DELETE` | `/api/sprints/{id}/tasks/{taskId}` | Remove a task from a sprint |

Sprint creation/update body:

```json
{
  "name": "Sprint 1",
  "startDate": "2026-10-22",
  "endDate": "2026-10-30",
  "projectId": 1
}
```

`endDate` must be on or after `startDate`; `projectId` is required. Sprint lists
support `projectId`, `unassigned=true`, `page`, and `pageSize`. A duplicate name
within the same project returns `409`. Adding a task to a sprint also adds it to
that sprint's project.

### Assistant (chat)

`POST /api/chat` accepts `{"message":"..."}` (trimmed, max 500 chars) and always
responds `200` with:

```json
{ "reply": "…", "task": null, "status": null, "changed": false }
```

`task` is populated and `changed` is `true` when the message created a task;
`status` is populated for a resolved status query. The message is routed by
intent:

- **Create** (`create|add|new task …`) — handled locally and deterministically.
  It needs a description, an assignee (`for <name>`), and a due date (`due` +
  `YYYY-MM-DD`, `today`, `tomorrow`, a weekday, or `in N days`). Optional
  `sprint "<name>"` and `project(s) <names>` clauses are resolved by exact then
  unique-prefix match; ambiguous names produce a clarifying reply. Example:

  ```json
  {"message":"create task \"Fix login bug\" for Alice due tomorrow in sprint \"Sprint 1\" projects Auth and Billing"}
  ```

- **Status** (contains `status`, `on track`, `at risk`, `blocked`, `how is`,
  `how's`, or `progress`) — handled locally. It resolves the named sprint or
  project and returns its health, or asks which one you mean. Example:

  ```json
  {"message":"what is the status of project Customer Portal?"}
  ```

- **Anything else** — a free-form question answered by Gemini when configured;
  otherwise a static help reply. See below.

### Project and sprint health

Health is computed entirely in Go (`internal/status`) — no AI, nothing leaves
the machine. The verdict is `on_track`, `at_risk`, or `blocked` with a reason
and task counts:

1. A scope with no tasks is **on track**.
2. Any open task with status `blocked` makes the scope **blocked**.
3. Any unfinished task past its due date makes it **at risk**.
4. For **sprints** only, it is also at risk when the sprint has ended with open
   tasks, when an unfinished task is due after the sprint's end, or when the
   share of sprint time elapsed is more than 25 percentage points ahead of the
   share of tasks done.
5. Otherwise it is **on track**.

## Gemini assistant behavior

The assistant answers **only free-form** chat questions; task creation and
status queries never reach Gemini. The client is `internal/gemini`, and its
behavior is bounded by design (defaults from the source):

- **Read-only context.** Gemini is given a system instruction plus a read-only
  summary — today's date, all projects and sprints, and up to **50 tasks** — and
  is instructed to answer only from that context and never to invent data. It
  cannot create, edit, or delete anything; its output is returned to the user as
  plain text and never drives a query or mutation.
- **Bounded output.** `maxOutputTokens = 256`, `temperature = 0.2`, plain text.
- **Thinking level.** For models that support it (the Gemini 3 family), the
  request sets `thinkingConfig.thinkingLevel = "low"` to minimize reasoning
  overhead. Models that do not support the parameter (for example the Gemini 2.5
  family) are left on their own defaults — the parameter is omitted so no
  invalid field is ever sent.
- **Timeouts.** Each HTTP attempt is capped at **15 s**; a single assistant turn
  is capped at **55 s** (`assistantTimeout`), and the server's `WriteTimeout`
  (65 s) exceeds that so a reply can still be written.
- **Retries.** Transient failures (HTTP 429/408, any 5xx, or network/timeout
  errors) are retried up to **2** extra times (3 attempts total) with jittered
  exponential backoff (base 500 ms, capped at 4 s). Permanent errors (bad key,
  permission denied, unknown model, malformed request) are not retried.
- **Rate limiting.** A local token-bucket limiter allows about **15 calls/minute
  with a burst of 5**, checked once per logical request (retries do not
  re-consume it), so the assistant stays within free-tier limits.
- **Quota exhausted / unavailable.** The handler never switches to a paid model
  or project. On a rate-limit/quota error it replies asking the user to wait a
  minute; on a timeout it asks them to try again; on other errors it returns a
  short "temporarily unavailable" message that points back to the deterministic
  create/status commands. The HTTP status stays `200` in all of these cases.
- **Key safety.** The key is sent in the `x-goog-api-key` header (never in a
  URL), is never logged, and never appears in an error message.

### Gemini model selection

There are two distinct notions of "the model":

- **Code default:** `gemini.DefaultModel = "gemini-3.8-flash"`, used when
  `GEMINI_MODEL` is unset.
- **Runtime selection:** whatever `GEMINI_MODEL` is set to in the environment
  overrides the default. The **currently deployed Render service sets
  `GEMINI_MODEL=gemini-3.5-flash-lite`**, so the live backend uses that model
  rather than the code default.

To use a different model, set `GEMINI_MODEL` to its name in the relevant
environment (local shell, `.env`, or the Render service settings) and restart or
redeploy. Model names, availability, free-tier eligibility, and pricing are
controlled by Google and can change — this project does not guarantee that any
particular model is available or free. Pick a model your API key can access; an
unknown or inaccessible model surfaces as an assistant error (a permanent,
non-retried failure) rather than a crash.

## Tests

```bash
gofmt -l .        # lists files needing formatting (empty = clean)
go vet ./...
go test ./...
```

The PostgreSQL integration tests are **skipped unless `TEST_DATABASE_URL` is
set**. Run them against a **separate throwaway database**, never your
development database — the tests create and clean up records in it:

```bash
createdb -h 127.0.0.1 -p 5432 -U "$PGUSER" taskmanager_test
psql -h 127.0.0.1 -p 5432 -U "$PGUSER" \
  -d taskmanager_test -v ON_ERROR_STOP=1 -f db/schema.sql
export TEST_DATABASE_URL="postgres://127.0.0.1:5432/taskmanager_test?sslmode=disable"
go test -count=1 ./...
```

As with the app, the test connection also reads `PGUSER`/`PGPASSWORD` from the
environment when they are not embedded in `TEST_DATABASE_URL`.

## Deployment (Render)

The backend runs as a Render web service. There is no `render.yaml` in the
repository; the service is configured in the Render dashboard. Using Render's
native Go environment (no Docker):

| Setting | Value |
|---|---|
| Language / environment | Go |
| Build command | `go build -o app .` |
| Start command | `./app` |

Render assigns the service a port through the `PORT` environment variable
(default `10000`). This service reads **`HTTP_ADDR`**, not `PORT`, so set
`HTTP_ADDR` to bind all interfaces on that port, e.g. `HTTP_ADDR=0.0.0.0:10000`,
and keep the Render port at the same value. (Binding beyond loopback logs the
no-authentication warning — expected here, since Render terminates TLS in front
of the service.)

Service environment variables:

| Variable | Example value | Notes |
|---|---|---|
| `HTTP_ADDR` | `0.0.0.0:10000` | Must match Render's assigned port. |
| `DATABASE_URL` | *(managed Postgres connection string)* | Use Render's managed PostgreSQL or another provider; apply `db/schema.sql` to it once (and after schema changes). Keep `sslmode` appropriate for the provider. |
| `CORS_ORIGIN` | `https://<your-frontend>.vercel.app` | The exact deployed frontend origin, so the browser's cross-origin calls are allowed. |
| `GEMINI_API_KEY` | *(secret)* | Set to enable the assistant; add it as a secret env var, never in a committed file. |
| `GEMINI_MODEL` | `gemini-3.5-flash-lite` | Currently set on the live service (see [Gemini model selection](#gemini-model-selection)). Optional; omit to use the code default. |

Apply the schema to the managed database before first use, for example from your
machine (or Render's shell) with `psql "$DATABASE_URL" -f db/schema.sql`. This
README documents configuration only; it does not trigger a deploy.

## Project layout

```text
main.go                 # Config, PostgreSQL pool, assistant wiring, HTTP server lifecycle
internal/api/           # HTTP routes, handlers, validation, query parsing, chat, CORS
internal/gemini/        # Dependency-free Gemini generateContent client
internal/store/         # PostgreSQL persistence and queries (+ integration tests)
internal/classify/      # Deterministic keyword-based task categorization
internal/status/        # Derived project/sprint health rules
db/schema.sql           # Idempotent schema and in-place migrations
go.mod, go.sum          # Go module and dependency locks
```

## Related repository

- **Frontend:** [task-manager-frontend](https://github.com/devaanggoswami-debug/task-manager-frontend)
  — the Next.js web client for this API.
