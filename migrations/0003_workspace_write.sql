ALTER TABLE workspaces ADD COLUMN write_enabled INTEGER NOT NULL DEFAULT 0 CHECK(write_enabled IN (0, 1));
ALTER TABLE tasks ADD COLUMN execution_mode TEXT NOT NULL DEFAULT 'read-only' CHECK(execution_mode IN ('read-only', 'workspace-write'));
CREATE INDEX approvals_write_ready ON approvals(operation, status, expires_at, created_at);
