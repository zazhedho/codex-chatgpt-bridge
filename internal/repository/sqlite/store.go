package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/codex-chatgpt-bridge/migrations"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("database directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure database directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve database directory: %w", err)
	}
	path := filepath.Join(canonical, "bridge.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	legacy, err := isLegacyTypeScriptDB(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if legacy {
		db.Close()
		return nil, errors.New("refusing TypeScript database; configure a separate BRIDGE_DATA_DIR")
	}
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.secureFiles(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func isLegacyTypeScriptDB(db *sql.DB) (bool, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return false, fmt.Errorf("inspect database schema: %w", err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return tables["workspaces"] && tables["tasks"] && !tables["schema_migrations"], nil
}

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied != 0 {
			continue
		}
		sqlBytes, err := migrations.Files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, unixepoch())`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func (s *Store) secureFiles() error {
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure database file: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }
