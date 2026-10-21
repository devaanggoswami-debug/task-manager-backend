
CREATE TABLE IF NOT EXISTS tasks (
  id          BIGSERIAL PRIMARY KEY,
  assignee    VARCHAR(100)  NOT NULL CHECK (length(btrim(assignee)) > 0),
  due_date    DATE          NOT NULL,
  description VARCHAR(1000) NOT NULL CHECK (length(btrim(description)) > 0),
  created_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ   NOT NULL DEFAULT now()
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'not_started';
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tasks' AND column_name = 'done') THEN
    UPDATE tasks SET status = 'done' WHERE done;
    ALTER TABLE tasks DROP COLUMN done;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_status_check') THEN
    ALTER TABLE tasks ADD CONSTRAINT tasks_status_check
      CHECK (status IN ('not_started','in_progress','blocked','done'));
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS task_comments (
  id         BIGSERIAL PRIMARY KEY,
  task_id    BIGINT        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author     VARCHAR(100)  NOT NULL CHECK (length(btrim(author)) > 0),
  body       VARCHAR(2000) NOT NULL CHECK (length(btrim(body)) > 0),
  created_at TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS task_comments_task_id_idx ON task_comments (task_id, id);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS category VARCHAR(20);
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_category_check') THEN
    ALTER TABLE tasks ADD CONSTRAINT tasks_category_check
      CHECK (category IS NULL OR category IN ('engineering','marketing','finance','sales','compliance'));
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS sprints (
  id         BIGSERIAL PRIMARY KEY,
  name       VARCHAR(100) NOT NULL CHECK (length(btrim(name)) > 0),
  start_date DATE         NOT NULL,
  end_date   DATE         NOT NULL,
  created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
  CHECK (end_date >= start_date)
);

CREATE TABLE IF NOT EXISTS projects (
  id         BIGSERIAL PRIMARY KEY,
  name       VARCHAR(100) NOT NULL CHECK (length(btrim(name)) > 0),
  created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS projects_name_lower_idx ON projects (lower(name));

CREATE TABLE IF NOT EXISTS sprint_tasks (
  sprint_id BIGINT NOT NULL REFERENCES sprints(id) ON DELETE CASCADE,
  task_id   BIGINT NOT NULL REFERENCES tasks(id)   ON DELETE CASCADE,
  PRIMARY KEY (sprint_id, task_id)
);
CREATE INDEX IF NOT EXISTS sprint_tasks_task_idx ON sprint_tasks (task_id);

CREATE TABLE IF NOT EXISTS project_tasks (
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  task_id    BIGINT NOT NULL REFERENCES tasks(id)    ON DELETE CASCADE,
  PRIMARY KEY (project_id, task_id)
);
CREATE INDEX IF NOT EXISTS project_tasks_task_idx ON project_tasks (task_id);

ALTER TABLE sprints ADD COLUMN IF NOT EXISTS project_id BIGINT REFERENCES projects(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS sprints_project_idx ON sprints (project_id, start_date, id);
DROP INDEX IF EXISTS sprints_name_lower_idx;
CREATE UNIQUE INDEX IF NOT EXISTS sprints_project_name_lower_idx ON sprints (COALESCE(project_id, 0), lower(name));
