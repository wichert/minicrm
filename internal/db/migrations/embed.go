// Package migrations embeds SQL database migrations and provides a runner.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

// FS holds the embedded SQL migration files.
//
//go:embed *.sql
var FS embed.FS

// LegacyAccountIDSetting is the PostgreSQL session setting exposing the pre-multi-account
// mail account ID to the migrations.
const LegacyAccountIDSetting = "minicrm.legacy_account_id"

// Up applies all pending SQL migrations against db.
//
// legacyAccountID (typically the first configured mail account ID) is exposed to the
// migrations as the PostgreSQL session setting minicrm.legacy_account_id, so data created
// before multi-account support can be attributed to an existing account instead of an
// arbitrary hardcoded default.
func Up(ctx context.Context, db *sql.DB, legacyAccountID string) error {
	if legacyAccountID != "" {
		// Pin the pool to a single connection so the session-level setting applied
		// below is guaranteed to be visible to the migration statements goose runs.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if _, err := db.ExecContext(ctx,
			`SELECT set_config('`+LegacyAccountIDSetting+`', $1, false)`, legacyAccountID); err != nil {
			return fmt.Errorf("set %s: %w", LegacyAccountIDSetting, err)
		}
	}

	goose.SetBaseFS(FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("run goose migrations: %w", err)
	}
	return nil
}
