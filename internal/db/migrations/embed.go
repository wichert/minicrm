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

// ensureCitext installs the citext extension into the public schema once, before any
// migration runs.
//
// Every integration test creates its own schema and prepends it to search_path, so the
// extension must live in public (which they all keep on search_path) to be resolvable.
// CREATE EXTENSION IF NOT EXISTS is not safe under concurrency: PostgreSQL checks for an
// existing extension and creates it in separate steps, so parallel test packages (go test
// runs packages concurrently, and each migrates its own schema) can both pass the check and
// then collide, leaving the loser with "type citext does not exist". Pre-creating it here and
// treating a lost race as success removes that window for good.
func ensureCitext(ctx context.Context, db *sql.DB) error {
	installed := func() (bool, error) {
		var ok bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'citext')`).Scan(&ok)
		return ok, err
	}
	if ok, err := installed(); err != nil {
		return fmt.Errorf("check citext extension: %w", err)
	} else if ok {
		return nil
	}
	if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS citext SCHEMA public`); err != nil {
		// Another connection may have won the race; the extension existing is all we need.
		if ok, checkErr := installed(); checkErr == nil && ok {
			return nil
		}
		return fmt.Errorf("create citext extension: %w", err)
	}
	return nil
}

// Up applies all pending SQL migrations against db.
//
// legacyAccountID (typically the first configured mail account ID) is exposed to the
// migrations as the PostgreSQL session setting minicrm.legacy_account_id, so data created
// before multi-account support can be attributed to an existing account instead of an
// arbitrary hardcoded default.
func Up(ctx context.Context, db *sql.DB, legacyAccountID string) error {
	if err := ensureCitext(ctx, db); err != nil {
		return err
	}
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
