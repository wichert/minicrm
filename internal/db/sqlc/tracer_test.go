package sqlc_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

func TestStructuredQueryTracer(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	tracer := sqlc.NewStructuredQueryTracer(logger)

	actorID := uuid.New()
	ctx := sqlc.WithActor(context.Background(), actorID)
	ctx = sqlc.WithRequestMeta(ctx, "req-123", "192.0.2.1")

	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "SELECT id FROM threads WHERE id = $1",
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag("SELECT 1"),
	})

	out := buf.String()
	if !strings.Contains(out, `"event_category":"data_access"`) {
		t.Fatalf("expected data_access category, got: %s", out)
	}
	if !strings.Contains(out, `"operation":"read"`) {
		t.Fatalf("expected read operation, got: %s", out)
	}
	if !strings.Contains(out, actorID.String()) {
		t.Fatalf("expected actor_user_id %s, got: %s", actorID, out)
	}

	buf.Reset()
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "UPDATE threads SET status = 'completed' WHERE id = $1",
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag("UPDATE 1"),
	})
	out = buf.String()
	if !strings.Contains(out, `"event_category":"data_change"`) || !strings.Contains(out, `"operation":"update"`) {
		t.Fatalf("expected data_change/update, got: %s", out)
	}
}
