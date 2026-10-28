-- +goose Up
ALTER TABLE thread_events ADD COLUMN event_order BIGINT GENERATED ALWAYS AS IDENTITY;
CREATE INDEX idx_thread_events_chronological ON thread_events (thread_id, occurred_at, event_order);

-- +goose Down
DROP INDEX idx_thread_events_chronological;
ALTER TABLE thread_events DROP COLUMN event_order;
