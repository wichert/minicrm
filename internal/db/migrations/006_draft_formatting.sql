-- +goose Up
ALTER TABLE reply_drafts ADD COLUMN formatting TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE reply_drafts DROP COLUMN formatting;
