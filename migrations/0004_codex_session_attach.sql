ALTER TABLE threads ADD COLUMN source TEXT NOT NULL DEFAULT 'bridge';
ALTER TABLE threads ADD COLUMN session_path TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;

UPDATE threads SET updated_at = created_at WHERE updated_at = 0;

ALTER TABLE audit_events ADD COLUMN resource_id TEXT NOT NULL DEFAULT '';

CREATE TABLE session_attach_requests (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  workspace_path TEXT NOT NULL,
  root_device INTEGER NOT NULL,
  root_inode INTEGER NOT NULL,
  session_updated_at INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'rejected', 'expired')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  decided_at INTEGER,
  approved_by TEXT
);
CREATE UNIQUE INDEX session_attach_pending_thread ON session_attach_requests(thread_id) WHERE status='pending';
CREATE INDEX session_attach_pending_expiry ON session_attach_requests(status, expires_at);
