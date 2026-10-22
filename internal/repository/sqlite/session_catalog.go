package sqlite

import (
	"context"
	"database/sql"
)

func (s *Store) SessionCatalogEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT global_session_catalog_enabled FROM bridge_settings WHERE id=1`).Scan(&enabled)
	return enabled, err
}

func (s *Store) SetSessionCatalogEnabled(ctx context.Context, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE bridge_settings SET global_session_catalog_enabled=? WHERE id=1`, enabled)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return s.secureFiles()
}
