-- A secret change in progress on a sandbox: an attach or detach holds this
-- row from its binding write through its guest update, so no snapshot
-- records a binding the change may yet undo and no second change interleaves.
-- A row past expires_at is free to take, so a holder that dies cannot keep it.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10s';

CREATE TABLE IF NOT EXISTS sandbox_secret_write_hold (
    sandbox_id uuid PRIMARY KEY REFERENCES sandbox(id) ON DELETE CASCADE,
    holder     uuid NOT NULL,
    expires_at timestamptz NOT NULL
);

ALTER TABLE sandbox_secret_write_hold ENABLE ROW LEVEL SECURITY;

COMMIT;
