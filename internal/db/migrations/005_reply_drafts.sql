-- +goose Up
CREATE TABLE reply_drafts (
    id BIGSERIAL PRIMARY KEY,
    thread_id UUID NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
    revision UUID NOT NULL UNIQUE,
    body TEXT NOT NULL,
    session_id TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    discarded BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX reply_drafts_thread_idx ON reply_drafts (thread_id, id DESC);
ALTER TABLE thread_events DROP CONSTRAINT thread_events_event_type_check;
ALTER TABLE thread_events ADD CONSTRAINT thread_events_event_type_check CHECK (event_type IN (
    'created', 'status_changed', 'assigned', 'reassigned',
    'reply_received', 'reply_sent', 'reply_sent_external', 'reopen_merged', 'draft_discarded'
));

-- +goose Down
DELETE FROM thread_events WHERE event_type = 'draft_discarded';
ALTER TABLE thread_events DROP CONSTRAINT thread_events_event_type_check;
ALTER TABLE thread_events ADD CONSTRAINT thread_events_event_type_check CHECK (event_type IN (
    'created', 'status_changed', 'assigned', 'reassigned',
    'reply_received', 'reply_sent', 'reply_sent_external', 'reopen_merged'
));
DROP TABLE reply_drafts;
