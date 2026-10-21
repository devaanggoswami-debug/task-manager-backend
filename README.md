# Task Manager Backend

A Go HTTP REST API for a local project/task management application. It persists tasks, comments, projects, sprints, and their relationships in PostgreSQL. The frontend is maintained separately in [task-manager-frontend](https://github.com/devaanggoswami-debug/task-manager-frontend).

- **API base URL:** `http://127.0.0.1:8080`
- **Database:** PostgreSQL, default database name `taskmanager`
- **Repository:** [task-manager-backend](https://github.com/devaanggoswami-debug/task-manager-backend)
- **Schema:** [`db/schema.sql`](db/schema.sql)

> **Security:** The API has no authentication. By default it binds to `127.0.0.1:8080` and only allows the configured frontend origin. Do not bind it to a public/network interface without implementing authentication, authorization, and appropriate production security controls.

## Capabilities

- Task CRUD with assignee, due date, description, status, category, and project/sprint memberships.
- Task comments/updates, with cascading cleanup when a task is deleted.
- Project create, list, detail, and rename operations.
- Sprint create, list, detail, and update operations, with project ownership and task membership.
- Search, filters, and pagination for task/project/sprint/comment lists.
- Derived project and sprint health: `on_track`, `at_risk`, or `blocked`.
- Keyword-based task category classification.
- A rule-based chat endpoint that can create tasks and report project/sprint status. It does **not** call an external LLM or send task data to an AI provider.
- JSON validation/errors, CORS handling, request-size limits, graceful shutdown, and unit/integration tests.

## Tech stack

- Go `1.23+`
- Standard library `net/http` router and JSON handling
- PostgreSQL
- [`pgx/v5`](https://github.com/jackc/pgx) and `pgxpool` for database access

## Local setup (Windows with Git Bash)

Install Go 1.23 or newer and PostgreSQL. Ensure the PostgreSQL service is running and that the command-line utilities `psql`, `createdb`, and `pg_isready` are available. On this Windows setup, PostgreSQL's utilities may be located at `C:\Program Files\PostgreSQL\18\bin`.

### 1. Clone the repository

```bash
git clone https://github.com/devaanggoswami-debug/task-manager-backend.git
cd task-manager-backend
```

### 2. Configure the local PostgreSQL connection

For a typical local PostgreSQL installation, configure the environment in Git Bash:

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

The password is entered interactively and is not written to the repository. Keep it out of source files, command examples, and commits.

If the PostgreSQL service is stopped, open **PowerShell as Administrator** and run:

```powershell
Start-Service -Name postgresql-x64-18
```

Verify the server is accepting connections:

```bash
pg_isready -h 127.0.0.1 -p 5432
```

### 3. Create the database and apply the schema

The following commands create the database only if it does not already exist, then apply the idempotent schema/migration script:

```bash
if ! psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d postgres -Atqc \
  "SELECT 1 FROM pg_database WHERE datname = 'taskmanager'" | grep -q 1; then
  createdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" taskmanager
fi

psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" \
  -d taskmanager -v ON_ERROR_STOP=1 -f db/schema.sql
```

The schema is designed to be rerun. It creates/updates tables for:

- `tasks` — task details, status and category
- `task_comments` — task updates/comments
- `projects` and `sprints`
- `project_tasks` and `sprint_tasks` — task memberships

A sprint belongs to a project; a task can be associated with multiple projects and sprints. Categories are `engineering`, `marketing`, `finance`, and `sales`, or `compliance`. Task statuses are `not_started`, `in_progress`, `blocked`, and `done`.

### 4. Run the API

```bash
go run .
```

The API listens on **http://127.0.0.1:8080** by default. Keep this terminal open while the frontend is running. A simple health/data check is:

```bash
curl -i http://127.0.0.1:8080/api/tasks
```

The frontend expects the API at `http://127.0.0.1:8080` by default and normally runs on `http://127.0.0.1:3100`.

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgres://localhost:5432/taskmanager?sslmode=disable` | PostgreSQL connection string |
| `PGUSER` | Connection environment/default | PostgreSQL role used when not embedded in `DATABASE_URL` |
| `PGPASSWORD` | Unset | Optional database password; set it in the process environment, not in source control |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `CORS_ORIGIN` | `http://127.0.0.1:3100` | Allowed browser origin |

`DATABASE_URL` controls the host, port, database, and other connection-string settings. The default URL is `postgres://localhost:5432/taskmanager?sslmode=disable`; set it explicitly to `postgres://127.0.0.1:5432/taskmanager?sslmode=disable` for a local IPv4 connection if needed. When credentials are not embedded in the URL, pgx can use the `PGUSER`/`PGPASSWORD` environment settings. Avoid putting credentials in a committed URL or shell-history entry. The application logs a warning if `HTTP_ADDR` is configured to listen beyond loopback because this API has no authentication.

## HTTP API reference

All endpoints are rooted at `/api`. Requests and responses use JSON unless the response is `204 No Content`. The API returns validation errors as JSON, typically `{ "error": "validation failed", "fields": { ... } }`.

### Tasks

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/tasks` | List tasks; supports filters and pagination |
| `POST` | `/api/tasks` | Create a task |
| `GET` | `/api/tasks/{id}` | Get one task |
| `PUT` | `/api/tasks/{id}` | Replace/update task fields |
| `DELETE` | `/api/tasks/{id}` | Delete task (and its comments) |
| `GET` | `/api/tasks/{id}/comments` | List task updates/comments |
| `POST` | `/api/tasks/{id}/comments` | Add a comment/update |

Task list query parameters include `q` (case-insensitive search in assignee/description), `projectId`, `sprintId`, `excludeSprintId`, `status`, `category`, `page`, and `pageSize`. Valid statuses: `not_started`, `in_progress`, `blocked`, `done`. Valid categories: `engineering`, `marketing`, `finance`, `sales`, `compliance`, plus `unclassified` as a filter value. Pagination is 1-based, defaults to 20 items when enabled, and allows a maximum page size of 100. Paged list responses include the `X-Total-Count` header.

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

Required task fields are `assignee`, `date` (`YYYY-MM-DD`), and `description`. Status defaults to `not_started` when omitted on create. If category is omitted, the backend classifies the description using keyword rules.

### Projects

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/projects` | List/search projects |
| `POST` | `/api/projects` | Create a project (`{"name":"Customer Portal"}`) |
| `GET` | `/api/projects/{id}` | Get project details and counts |
| `PUT` | `/api/projects/{id}` | Rename a project |
| `GET` | `/api/projects/{id}/status` | Calculate project health |

Project list supports `q`, `page`, and `pageSize`.

### Sprints

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/sprints` | List sprints |
| `POST` | `/api/sprints` | Create a sprint |
| `GET` | `/api/sprints/{id}` | Get sprint details |
| `PUT` | `/api/sprints/{id}` | Update a sprint |
| `GET` | `/api/sprints/{id}/status` | Calculate sprint health |
| `POST` | `/api/sprints/{id}/tasks` | Add tasks to a sprint (`{"taskIds":[1,2]}`) |
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

Sprint lists support `projectId`, `unassigned=true`, `page`, and `pageSize`. Sprint names are unique within a project (case-insensitively).

### Assistant

`POST /api/chat` accepts a short `{"message":"..."}` request. It uses local rule-based parsing—no external AI service is called. Examples:

```json
{"message":"create task \"Fix login bug\" for Alice due tomorrow in sprint \"Sprint 1\""}
```

```json
{"message":"what is the status of project Customer Portal?"}
```

For task creation, the command needs a description, assignee (`for <name>`), and due date (`YYYY-MM-DD`, `today`, `tomorrow`, a weekday, or `in N days`). Project/sprint names must resolve unambiguously.

### Project and sprint health

Health responses use `on_track`, `at_risk`, or `blocked`, along with a reason and task counts. A blocked open task makes a scope blocked; overdue unfinished tasks make it at risk. Sprint health also considers sprint end dates, tasks due after sprint end, and progress relative to elapsed sprint time.

## Tests

### Unit/API tests

```bash
go vet ./...
go test ./...
```

The PostgreSQL integration tests are skipped unless `TEST_DATABASE_URL` is set. To run them safely, use a **separate test database**, never your development database:

```bash
createdb -h 127.0.0.1 -p 5432 -U "$PGUSER" taskmanager_test
psql -h 127.0.0.1 -p 5432 -U "$PGUSER" \
  -d taskmanager_test -v ON_ERROR_STOP=1 -f db/schema.sql
export TEST_DATABASE_URL="postgres://127.0.0.1:5432/taskmanager_test?sslmode=disable"
go test -count=1 ./...
```

The test connection also uses `PGUSER`/`PGPASSWORD` from the environment when those values are not embedded in the connection string. Integration tests create and clean up test records in `taskmanager_test`.

## Project layout

```text
main.go                      # Configuration, PostgreSQL pool, HTTP server lifecycle
internal/api/                 # HTTP routes, handlers, validation, query parsing, chat endpoint
internal/store/                # PostgreSQL persistence and query operations
internal/classify/             # Deterministic keyword-based task categorization
internal/status/                # Derived project/sprint health rules
db/schema.sql                   # Idempotent schema and migrations
go.mod, go.sum                  # Go module/dependency locks
```
