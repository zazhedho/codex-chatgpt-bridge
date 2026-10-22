CREATE TABLE session_project_bindings (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL DEFAULT '',
  trusted_root_id TEXT NOT NULL REFERENCES trusted_workspace_roots(id) ON DELETE RESTRICT,
  trusted_root_path TEXT NOT NULL,
  root_device INTEGER NOT NULL,
  root_inode INTEGER NOT NULL,
  session_path TEXT NOT NULL,
  session_device INTEGER NOT NULL,
  session_inode INTEGER NOT NULL,
  session_created_at INTEGER NOT NULL,
  session_updated_at INTEGER NOT NULL,
  project_path TEXT NOT NULL,
  project_device INTEGER NOT NULL,
  project_inode INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'rejected', 'expired', 'revoked')),
  requested_by TEXT NOT NULL,
  approved_by TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  decided_at INTEGER
);
CREATE UNIQUE INDEX session_project_binding_active_thread ON session_project_bindings(thread_id) WHERE status IN ('pending', 'approved');
CREATE INDEX session_project_binding_status_created ON session_project_bindings(status, created_at DESC);
