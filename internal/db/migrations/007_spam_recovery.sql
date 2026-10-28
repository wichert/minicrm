-- +goose Up
ALTER TABLE imap_jobs DROP CONSTRAINT imap_jobs_job_type_check;
ALTER TABLE imap_jobs ADD CONSTRAINT imap_jobs_job_type_check CHECK (job_type IN ('move_to_archive', 'move_to_junk', 'restore_from_junk', 'append_to_sent'));
CREATE INDEX idx_imap_jobs_email_order ON imap_jobs (account_id, email_id, enqueued_at, id) WHERE status IN ('pending', 'processing');

-- +goose Down
DROP INDEX idx_imap_jobs_email_order;
DELETE FROM imap_jobs WHERE job_type = 'restore_from_junk';
ALTER TABLE imap_jobs DROP CONSTRAINT imap_jobs_job_type_check;
ALTER TABLE imap_jobs ADD CONSTRAINT imap_jobs_job_type_check CHECK (job_type IN ('move_to_archive', 'move_to_junk', 'append_to_sent'));
