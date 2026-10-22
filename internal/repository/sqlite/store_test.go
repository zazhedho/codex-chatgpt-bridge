package sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenMigratesAndProtectsDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var migrations int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 7 {
		t.Fatalf("migration count = %d, want 7", migrations)
	}
	for _, file := range []string{store.path, store.path + "-wal", store.path + "-shm"} {
		info, err := os.Stat(file)
		if os.IsNotExist(err) && file != store.path {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("database file %s has mode %o", filepath.Base(file), info.Mode().Perm())
		}
	}
}

func TestNewColumnsKeepExistingWorkspacesAndTasksReadOnly(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`INSERT INTO workspaces(id, root_path, root_device, root_inode, created_at) VALUES ('fixture', '/tmp/fixture', 1, 2, 1)`); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.GetWorkspace(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.WriteEnabled {
		t.Fatal("existing workspace became writable by default")
	}
	if _, err := store.db.Exec(`INSERT INTO tasks(id, workspace_id, status, prompt_hash, created_at, updated_at) VALUES ('task', 'fixture', 'queued', 'hash', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(t.Context(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if task.ExecutionMode != "read-only" {
		t.Fatalf("existing task execution mode = %q, want read-only", task.ExecutionMode)
	}
}

func TestOpenRefusesLegacyTypeScriptDatabaseWithoutMigration(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "bridge.sqlite")
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("CREATE TABLE workspaces(id TEXT PRIMARY KEY); CREATE TABLE tasks(id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if store, err := Open(dir); err == nil {
		store.Close()
		t.Fatal("opened a TypeScript database as Go storage")
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("legacy database changed during rejected open")
	}
}

func TestMigrationEnforcesTaskWorkspaceForeignKey(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.db.Exec(`INSERT INTO tasks (id, workspace_id, status, prompt_hash, created_at, updated_at)
		VALUES ('task', 'missing', 'queued', 'hash', 1, 1)`)
	if err == nil {
		t.Fatal("task was inserted without a registered workspace")
	}
}
