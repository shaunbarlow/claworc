package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00015_noop_instance_discord_config: registry placeholder for the
// Instance.DiscordConfig column added for the per-agent Discord connection
// feature (see docs/discord-connections.md).
//
// Per docs/migrations.md, additive column changes are handled by
// AutoMigrateAll on boot and do not require a Goose migration. However,
// the CI "Migration Drift Check" guard in .github/workflows/control-plane.yml
// errors out whenever models/models.go changes without a new migration file,
// so we register a no-op here to satisfy that guard and keep the goose
// registry contiguous.
func init() {
	register(&goose.Migration{
		// Version 15, not 14: the qmd-memory-backend branch (PR #3) took 14
		// with 00014_noop_memory_qmd.go while this feature was in flight,
		// and duplicate goose versions panic at init.
		Version: 15,
		Source:  "00015_noop_instance_discord_config.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
	})
}
