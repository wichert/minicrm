-- +goose Up
-- +goose StatementBegin
-- The seed CLI records a per-account default-assignee change as its own audit event type.
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_event_type_check;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_event_type_check CHECK (event_type IN (
    'user_created', 'user_precreated', 'user_updated',
    'user_login', 'user_login_failed', 'user_logout',
    'settings_updated', 'account_settings_updated'
));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM audit_logs WHERE event_type = 'account_settings_updated';
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_event_type_check;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_event_type_check CHECK (event_type IN (
    'user_created', 'user_precreated', 'user_updated',
    'user_login', 'user_login_failed', 'user_logout',
    'settings_updated'
));
-- +goose StatementEnd
