package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
	"gorm.io/gorm"

	"github.com/gluk-w/claworc/control-plane/internal/database/models"
)

// 00019_retire_qmd_memory: QMD has been removed as an OpenClaw memory
// backend (see docs/memory-config.md "History: QMD retirement" and
// https://docs.openclaw.ai/concepts/memory-builtin#migrating-from-qmd);
// builtin is now the only engine Claworc configures. This migration:
//
//   - drops the retired instances.memory_backend / instances.memory_qmd
//     columns (superseded by instances.memory_settings, added additively
//     by AutoMigrate — no migration needed for that half)
//   - drops the retired shared_folders.qmd_index / shared_folders.qmd_pattern
//     columns (superseded by memory_index / memory_index_pattern, likewise
//     additive)
//   - deletes the retired default_memory_backend / default_memory_qmd
//     settings rows (superseded by default_memory_settings)
//
// Not reversible: the QMD-specific values (search_mode, update_interval,
// scope rules, ...) have no builtin equivalent, so there is nothing
// meaningful to carry forward into MemorySettings. Any agent that was
// actually running on the qmd backend silently falls back to builtin on its
// next config push, matching OpenClaw's own upstream QMD-removal behavior
// (agents keep working; they just lose the QMD-specific index).
func init() {
	register(&goose.Migration{
		Version: 19,
		Source:  "00019_retire_qmd_memory.go",
		UpFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return WithMigrator(ctx, tx, func(m gorm.Migrator, gdb *gorm.DB) error {
				// Pass the literal snake_case DB column name, not the retired Go
				// field name: LookUpField only resolves fields the current
				// models.go struct still declares, and these were removed from
				// it in this same change, so HasColumn/DropColumn would otherwise
				// fall through to using the unresolved argument verbatim as the
				// SQL column name (i.e. the wrong, PascalCase, name).
				for _, col := range []string{"memory_backend", "memory_qmd"} {
					if m.HasColumn(&models.Instance{}, col) {
						if err := m.DropColumn(&models.Instance{}, col); err != nil {
							return err
						}
					}
				}
				for _, col := range []string{"qmd_index", "qmd_pattern"} {
					if m.HasColumn(&models.SharedFolder{}, col) {
						if err := m.DropColumn(&models.SharedFolder{}, col); err != nil {
							return err
						}
					}
				}
				if err := gdb.Model(&models.Setting{}).
					Where("key IN ?", []string{"default_memory_backend", "default_memory_qmd"}).
					Delete(&models.Setting{}).Error; err != nil {
					return err
				}
				return nil
			})
		},
		DownFnContext: func(ctx context.Context, tx *sql.Tx) error {
			return fmt.Errorf("not reversible: QMD-specific settings have no builtin equivalent to restore")
		},
	})
}
