package store

import (
	"context"
	"fmt"
)

// BackupTo writes a consistent copy of the database using VACUUM INTO.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}
