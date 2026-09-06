package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00023_noop_instance_session_reset: registry placeholder for the
// Instance.SessionResetSettings column. Additive model changes are applied by
// AutoMigrateAll; this migration keeps the drift guard contiguous.
func init() {
	register(&goose.Migration{
		Version: 23,
		Source:  "00023_noop_instance_session_reset.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
	})
}
