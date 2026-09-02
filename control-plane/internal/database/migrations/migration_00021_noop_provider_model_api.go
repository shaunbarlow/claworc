package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// 00021_noop_provider_model_api: registry placeholder for the ProviderModel.API
// field, which lets a single model override its provider's api adapter so
// OpenAI reasoning models can be declared openai-responses (their function
// tools are rejected on /v1/chat/completions alongside reasoning_effort).
//
// No schema change is involved: ProviderModel is serialized as JSON inside the
// existing llm_providers.models column rather than mapped to columns, and the
// field is tagged omitempty, so existing rows stay byte-identical and need no
// backfill. The migrationcheck tool passes without this file.
//
// It exists only for the CI "Migration Drift Check" guard in
// .github/workflows/control-plane.yml, which errors whenever
// internal/database/models/*.go changes without a new migration file being
// added, so we register a no-op to satisfy it and keep the goose registry
// contiguous. Same pattern as 00020_noop_openbao_secrets.go.
func init() {
	register(&goose.Migration{
		Version: 21,
		Source:  "00021_noop_provider_model_api.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return nil
		},
	})
}
