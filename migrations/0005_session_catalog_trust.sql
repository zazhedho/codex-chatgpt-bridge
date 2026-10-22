ALTER TABLE workspaces ADD COLUMN session_trusted INTEGER NOT NULL DEFAULT 0 CHECK(session_trusted IN (0, 1));

CREATE TABLE bridge_settings (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  global_session_catalog_enabled INTEGER NOT NULL DEFAULT 0 CHECK(global_session_catalog_enabled IN (0, 1))
);

INSERT INTO bridge_settings(id, global_session_catalog_enabled) VALUES (1, 0);
