-- +goose Up
ALTER TABLE system_settings ADD COLUMN attention_age_days INT NOT NULL DEFAULT 2 CHECK (attention_age_days > 0);

-- +goose Down
ALTER TABLE system_settings DROP COLUMN attention_age_days;
