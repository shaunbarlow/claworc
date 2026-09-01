package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00020_noop_openbao_secrets: registry placeholder for the model changes
// added by the optional Claworc-managed OpenBao secrets integration:
// Instance.SecretGrants, Instance.OpenbaoToken, and the new
// SharedSecretSet model.
//
// Per docs/migrations.md, additive column/table changes are handled by
// AutoMigrateAll on boot and do not require a Goose migration. However,
// the CI "Migration Drift Check" guard in .github/workflows/control-plane.yml
// errors out whenever models/models.go changes without a new migration file,
// so we register a no-op here to satisfy that guard and keep the goose
// registry contiguous. Same pattern as 00018_noop_instance_connector_token.go.
func init() {
	register(&goose.Migration{
		Version: 20,
		Source:  "00020_noop_openbao_secrets.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
	})
}
