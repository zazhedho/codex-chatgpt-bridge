CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  root_path TEXT NOT NULL UNIQUE,
  root_device INTEGER NOT NULL,
  root_inode INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  git_repository TEXT NOT NULL DEFAULT ''
);

CREATE TABLE workspace_requests (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  root_path TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'rejected', 'expired')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX workspace_requests_pending_id ON workspace_requests(workspace_id) WHERE status='pending';
CREATE UNIQUE INDEX workspace_requests_pending_path ON workspace_requests(root_path) WHERE status='pending';

CREATE TABLE threads (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  created_at INTEGER NOT NULL,
  title TEXT NOT NULL DEFAULT ''
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  thread_id TEXT REFERENCES threads(id) ON DELETE RESTRICT,
  turn_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('queued', 'running', 'waiting_approval', 'completed', 'failed', 'cancelled', 'interrupted')),
  prompt_hash TEXT NOT NULL,
  progress_json TEXT NOT NULL DEFAULT '[]',
  result TEXT,
  diff TEXT,
  error TEXT,
  output_expired INTEGER NOT NULL DEFAULT 0 CHECK(output_expired IN (0, 1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX tasks_workspace_created ON tasks(workspace_id, created_at DESC);
CREATE INDEX tasks_status ON tasks(status);

CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT,
  task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
  operation TEXT NOT NULL,
  target TEXT NOT NULL,
  parameters_json TEXT NOT NULL DEFAULT '{}',
  parameters_hash TEXT NOT NULL,
  artifact_hash TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'rejected', 'expired', 'consumed')),
  approved_by TEXT,
  expires_at INTEGER NOT NULL,
  decided_at INTEGER,
  consumed_at INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX approvals_pending ON approvals(status, expires_at);

CREATE TABLE audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  occurred_at INTEGER NOT NULL,
  operation TEXT NOT NULL,
  principal TEXT NOT NULL,
  workspace_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL,
  error_class TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_events_time ON audit_events(occurred_at DESC);
