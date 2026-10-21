-- +goose Up
-- +goose StatementBegin
-- Replies sent from another mail client (found in the Sent folder) are recorded as their own event type.
ALTER TABLE thread_events DROP CONSTRAINT thread_events_event_type_check;
ALTER TABLE thread_events ADD CONSTRAINT thread_events_event_type_check CHECK (event_type IN (
    'created', 'status_changed', 'assigned', 'reassigned',
    'reply_received', 'reply_sent', 'reply_sent_external', 'reopen_merged'
));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM thread_events WHERE event_type = 'reply_sent_external';
ALTER TABLE thread_events DROP CONSTRAINT thread_events_event_type_check;
ALTER TABLE thread_events ADD CONSTRAINT thread_events_event_type_check CHECK (event_type IN (
    'created', 'status_changed', 'assigned', 'reassigned',
    'reply_received', 'reply_sent', 'reopen_merged'
));
-- +goose StatementEnd
