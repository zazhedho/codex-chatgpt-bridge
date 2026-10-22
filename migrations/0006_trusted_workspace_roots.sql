CREATE TABLE trusted_workspace_roots (
  id TEXT PRIMARY KEY,
  root_path TEXT NOT NULL,
  root_device INTEGER NOT NULL,
  root_inode INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending', 'approved', 'disabled', 'revoked', 'expired')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  decided_at INTEGER,
  approved_by TEXT
);
CREATE UNIQUE INDEX trusted_root_path_active ON trusted_workspace_roots(root_path) WHERE status IN ('pending', 'approved', 'disabled');
CREATE INDEX trusted_root_status_created ON trusted_workspace_roots(status, created_at DESC);

ALTER TABLE workspaces ADD COLUMN trusted_root_id TEXT REFERENCES trusted_workspace_roots(id);
ALTER TABLE workspaces ADD COLUMN root_session_only INTEGER NOT NULL DEFAULT 0 CHECK(root_session_only IN (0, 1));
ALTER TABLE threads ADD COLUMN trusted_root_id TEXT NOT NULL DEFAULT '';
