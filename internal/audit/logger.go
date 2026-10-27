// Package audit provides immutable database audit trail recording and structured access/mutation logging.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

// Store is the persistence interface required by the audit Logger.
type Store interface {
	InsertAuditLog(ctx context.Context, entry sqlc.AuditLog) error
	InsertThreadEvent(ctx context.Context, ev sqlc.ThreadEvent) error
}

// Logger records immutable audit rows in PostgreSQL and emits structured slog entries.
type Logger struct {
	store Store
	slog  *slog.Logger
}

// NewLogger creates an audit Logger backed by store and slogLogger.
func NewLogger(store Store, slogLogger *slog.Logger) *Logger {
	if slogLogger == nil {
		slogLogger = slog.Default()
	}
	return &Logger{
		store: store,
		slog:  slogLogger,
	}
}

// WithStore creates a logger using the same structured logger and a different persistence store.
func (l *Logger) WithStore(store Store) *Logger {
	return NewLogger(store, l.slog)
}

// SystemEventParams describes a system, user, authentication, or settings audit event.
type SystemEventParams struct {
	ActorUserID  *uuid.UUID
	TargetUserID *uuid.UUID
	EventType    string
	IPAddress    string
	UserAgent    string
	OldValue     any
	NewValue     any
}

// LogSystemEvent persists an entry in audit_logs and emits a structured data_change log.
func (l *Logger) LogSystemEvent(ctx context.Context, p SystemEventParams) error {
	oldJSON, err := marshalOptional(p.OldValue)
	if err != nil {
		return fmt.Errorf("marshal old_value: %w", err)
	}
	newJSON, err := marshalOptional(p.NewValue)
	if err != nil {
		return fmt.Errorf("marshal new_value: %w", err)
	}

	if l.store != nil {
		err = l.store.InsertAuditLog(ctx, sqlc.AuditLog{
			ActorUserID:  p.ActorUserID,
			TargetUserID: p.TargetUserID,
			EventType:    p.EventType,
			IPAddress:    p.IPAddress,
			UserAgent:    p.UserAgent,
			OldValue:     oldJSON,
			NewValue:     newJSON,
		})
		if err != nil {
			return fmt.Errorf("insert audit_log: %w", err)
		}
	}

	actorStr := sqlc.ActorFromContext(ctx)
	if p.ActorUserID != nil {
		actorStr = p.ActorUserID.String()
	}
	targetStr := ""
	if p.TargetUserID != nil {
		targetStr = p.TargetUserID.String()
	}

	l.slog.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("event_type", p.EventType),
		slog.String("actor_user_id", actorStr),
		slog.String("target_user_id", targetStr),
		slog.String("ip_address", p.IPAddress),
	)
	return nil
}

// ThreadEventParams describes an immutable thread lifecycle or assignment event.
type ThreadEventParams struct {
	ThreadID    uuid.UUID
	ActorUserID *uuid.UUID
	EventType   string
	OldValue    any
	NewValue    any
}

// LogThreadEvent persists an entry in thread_events and emits a structured data_change log.
func (l *Logger) LogThreadEvent(ctx context.Context, p ThreadEventParams) error {
	oldJSON, err := marshalOptional(p.OldValue)
	if err != nil {
		return fmt.Errorf("marshal thread old_value: %w", err)
	}
	newJSON, err := marshalOptional(p.NewValue)
	if err != nil {
		return fmt.Errorf("marshal thread new_value: %w", err)
	}

	if l.store != nil {
		err = l.store.InsertThreadEvent(ctx, sqlc.ThreadEvent{
			ThreadID:    p.ThreadID,
			ActorUserID: p.ActorUserID,
			EventType:   p.EventType,
			OldValue:    oldJSON,
			NewValue:    newJSON,
		})
		if err != nil {
			return fmt.Errorf("insert thread_event: %w", err)
		}
	}

	actorStr := sqlc.ActorFromContext(ctx)
	if p.ActorUserID != nil {
		actorStr = p.ActorUserID.String()
	}

	l.slog.InfoContext(ctx, "data_change",
		slog.String("event_category", "data_change"),
		slog.String("resource_type", "thread"),
		slog.String("resource_id", p.ThreadID.String()),
		slog.String("event_type", p.EventType),
		slog.String("actor_user_id", actorStr),
	)
	return nil
}

// LogDataAccess emits a structured data_access log entry when sensitive CRM resources are read.
func (l *Logger) LogDataAccess(ctx context.Context, resourceType, resourceID string, attrs ...slog.Attr) {
	baseAttrs := make([]slog.Attr, 0, 5+len(attrs))
	baseAttrs = append(baseAttrs,
		slog.String("event_category", "data_access"),
		slog.String("operation", "read"),
		slog.String("resource_type", resourceType),
		slog.String("resource_id", resourceID),
		slog.String("actor_user_id", sqlc.ActorFromContext(ctx)),
	)
	if reqID := sqlc.RequestIDFromContext(ctx); reqID != "" {
		baseAttrs = append(baseAttrs, slog.String("request_id", reqID))
	}
	baseAttrs = append(baseAttrs, attrs...)
	l.slog.LogAttrs(ctx, slog.LevelInfo, "data_access", baseAttrs...)
}

func marshalOptional(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
