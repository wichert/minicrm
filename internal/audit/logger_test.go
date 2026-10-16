package audit_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/db/sqlc"
)

type fakeAuditStore struct {
	auditLogs    []sqlc.AuditLog
	threadEvents []sqlc.ThreadEvent
}

func (f *fakeAuditStore) InsertAuditLog(_ context.Context, entry sqlc.AuditLog) error {
	f.auditLogs = append(f.auditLogs, entry)
	return nil
}

func (f *fakeAuditStore) InsertThreadEvent(_ context.Context, ev sqlc.ThreadEvent) error {
	f.threadEvents = append(f.threadEvents, ev)
	return nil
}

func TestAuditLoggerRecordsSystemThreadAndAccessEvents(t *testing.T) {
	var buf bytes.Buffer
	slogLogger := slog.New(slog.NewJSONHandler(&buf, nil))
	store := &fakeAuditStore{}
	logger := audit.NewLogger(store, slogLogger)

	actorID := uuid.New()
	targetID := uuid.New()
	ctx := sqlc.WithActor(context.Background(), actorID)

	if err := logger.LogSystemEvent(ctx, audit.SystemEventParams{
		ActorUserID:  &actorID,
		TargetUserID: &targetID,
		EventType:    "user_precreated",
		IPAddress:    "192.0.2.10",
		NewValue:     map[string]string{"email": "bob@example.com"},
	}); err != nil {
		t.Fatalf("LogSystemEvent: %v", err)
	}

	if len(store.auditLogs) != 1 || store.auditLogs[0].EventType != "user_precreated" {
		t.Fatalf("unexpected auditLogs: %+v", store.auditLogs)
	}

	threadID := uuid.New()
	if err := logger.LogThreadEvent(ctx, audit.ThreadEventParams{
		ThreadID:    threadID,
		ActorUserID: &actorID,
		EventType:   "status_changed",
		OldValue:    map[string]string{"status": "new"},
		NewValue:    map[string]string{"status": "in_progress"},
	}); err != nil {
		t.Fatalf("LogThreadEvent: %v", err)
	}
	if len(store.threadEvents) != 1 || store.threadEvents[0].EventType != "status_changed" {
		t.Fatalf("unexpected threadEvents: %+v", store.threadEvents)
	}

	logger.LogDataAccess(ctx, "email_body", uuid.NewString())
	out := buf.String()
	if !strings.Contains(out, `"event_category":"data_access"`) || !strings.Contains(out, `"event_category":"data_change"`) {
		t.Fatalf("expected structured access and change logs, got: %s", out)
	}
}
