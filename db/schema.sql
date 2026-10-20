
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
