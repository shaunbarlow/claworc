package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00022_noop_instance_connector_opt_in: registry placeholder for the
// Instance.ConnectorMCPEnabled / Instance.ConnectorAdminAccessEnabled
// columns added to make the managed OpenConnector integration opt-in per
// agent instead of automatic for every instance whenever connector_enabled
// is on globally (see internal/handlers/connector.go).
//
// Per docs/migrations.md, additive column changes are handled by
// AutoMigrateAll on boot and do not require a Goose migration. However,
// the CI "Migration Drift Check" guard in .github/workflows/control-plane.yml
// errors out whenever models/models.go changes without a new migration file,
// so we register a no-op here to satisfy that guard and keep the goose
// registry contiguous.
func init() {
	register(&goose.Migration{
		Version: 22,
		Source:  "00022_noop_instance_connector_opt_in.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
	})
}
