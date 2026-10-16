package sqlc

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ctxKey string

const (
	actorKey     ctxKey = "minicrm_actor_id"
	requestIDKey ctxKey = "minicrm_request_id"
	clientIPKey  ctxKey = "minicrm_client_ip"
	startTimeKey ctxKey = "minicrm_query_start"
	sqlTextKey   ctxKey = "minicrm_query_sql"
)

// WithActor attaches an actor user ID to context for structured audit/query logging.
func WithActor(ctx context.Context, actorID uuid.UUID) context.Context {
	return context.WithValue(ctx, actorKey, actorID.String())
}

// ActorFromContext extracts the actor user ID string from context, or "system" if unset.
func ActorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey).(string); ok && v != "" {
		return v
	}
	return "system"
}

// WithRequestMeta attaches request correlation metadata to context.
func WithRequestMeta(ctx context.Context, requestID, clientIP string) context.Context {
	ctx = context.WithValue(ctx, requestIDKey, requestID)
	ctx = context.WithValue(ctx, clientIPKey, clientIP)
	return ctx
}

// RequestIDFromContext returns the request ID stored in ctx, if any.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// ClientIPFromContext returns the client IP stored in ctx, if any.
func ClientIPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPKey).(string); ok {
		return v
	}
	return ""
}

// StructuredQueryTracer implements pgx.QueryTracer to log all data access and changes via log/slog.
type StructuredQueryTracer struct {
	Logger *slog.Logger
}

// NewStructuredQueryTracer creates a QueryTracer using the given slog.Logger (or slog.Default()).
func NewStructuredQueryTracer(logger *slog.Logger) *StructuredQueryTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &StructuredQueryTracer{Logger: logger}
}

// TraceQueryStart records the start time and SQL text in ctx.
func (t *StructuredQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = context.WithValue(ctx, startTimeKey, time.Now())
	ctx = context.WithValue(ctx, sqlTextKey, data.SQL)
	return ctx
}

// TraceQueryEnd emits a structured log entry categorizing the query as data_access or data_change.
func (t *StructuredQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	var durationMs float64
	if start, ok := ctx.Value(startTimeKey).(time.Time); ok {
		durationMs = float64(time.Since(start).Microseconds()) / 1000.0
	}
	sqlText, _ := ctx.Value(sqlTextKey).(string)
	category, op := classifySQL(sqlText, data.CommandTag.String())

	attrs := []slog.Attr{
		slog.String("event_category", category),
		slog.String("operation", op),
		slog.String("command_tag", data.CommandTag.String()),
		slog.Int64("rows_affected", data.CommandTag.RowsAffected()),
		slog.Float64("duration_ms", durationMs),
		slog.String("actor_user_id", ActorFromContext(ctx)),
	}
	if reqID := RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String("request_id", reqID))
	}
	if ip := ClientIPFromContext(ctx); ip != "" {
		attrs = append(attrs, slog.String("client_ip", ip))
	}
	if data.Err != nil {
		attrs = append(attrs, slog.String("error", data.Err.Error()))
		t.Logger.LogAttrs(ctx, slog.LevelError, category, attrs...)
		return
	}
	t.Logger.LogAttrs(ctx, slog.LevelInfo, category, attrs...)
}

func classifySQL(sqlText, cmdTag string) (category, op string) {
	trimmed := strings.TrimSpace(strings.ToUpper(sqlText))
	switch {
	case strings.HasPrefix(trimmed, "SELECT"), strings.HasPrefix(cmdTag, "SELECT"):
		return "data_access", "read"
	case strings.HasPrefix(trimmed, "INSERT"), strings.HasPrefix(cmdTag, "INSERT"):
		return "data_change", "create"
	case strings.HasPrefix(trimmed, "UPDATE"), strings.HasPrefix(cmdTag, "UPDATE"):
		return "data_change", "update"
	case strings.HasPrefix(trimmed, "DELETE"), strings.HasPrefix(cmdTag, "DELETE"):
		return "data_change", "delete"
	default:
		return "data_access", "query"
	}
}
