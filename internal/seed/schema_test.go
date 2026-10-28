package seed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wichert/minicrm/internal/db/migrations"
)

// A missing schema is transient: the wait loop keeps probing until migrations appear.
func TestWaitForSchemaRetriesUntilReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attempts := 0
	probe := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return ErrSchemaMissing
		}
		return nil
	}
	if err := waitForSchema(ctx, probe, nil); err != nil {
		t.Fatalf("waitForSchema: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("probe calls = %d, want 3", attempts)
	}
}

// An outdated schema is a real error and must not be retried.
func TestWaitForSchemaFailsFastOnStaleSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stale := fmt.Errorf("database schema version %d is older than 6", 3)
	calls := 0
	probe := func(context.Context) error {
		calls++
		return stale
	}
	err := waitForSchema(ctx, probe, nil)
	if !errors.Is(err, stale) {
		t.Fatalf("got %v, want the stale-schema error", err)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (no retry)", calls)
	}
}

// The wait loop must respect its context instead of retrying forever.
func TestWaitForSchemaHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	probe := func(context.Context) error { return ErrSchemaMissing }
	err := waitForSchema(ctx, probe, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline error", err)
	}
	if !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("error should retain the last probe error: %v", err)
	}
}

func TestSchemaExistsRequiresAPool(t *testing.T) {
	if err := SchemaExists(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil pool")
	}
}

// RequiredSchemaVersion must track the newest embedded migration, otherwise the seed CLI
// would accept a database that is missing columns it relies on.
func TestRequiredSchemaVersionMatchesNewestMigration(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	highest := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("unexpected migration filename %q", name)
		}
		n, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("parse migration version from %q: %v", name, err)
		}
		highest = max(highest, n)
	}
	if highest != RequiredSchemaVersion {
		t.Fatalf("RequiredSchemaVersion = %d, but the newest migration is %d", RequiredSchemaVersion, highest)
	}
}
