-- CLI device login (superserve login). Rows always live in the default cell,
-- but the approving user's team may live in another cell, so neither user_id
-- nor team_id carries an FK and team_region says where to mint the key.
-- device_code now holds the SHA-256 of the secret the CLI polls with; the
-- API key itself is minted on the first poll after approval and never stored.
ALTER TABLE device_code DROP CONSTRAINT IF EXISTS device_code_user_id_fkey;
ALTER TABLE device_code DROP CONSTRAINT IF EXISTS device_code_status_check;
ALTER TABLE device_code
    ADD COLUMN IF NOT EXISTS client_name    text,
    ADD COLUMN IF NOT EXISTS requester_hash text,
    ADD COLUMN IF NOT EXISTS team_id        uuid,
    ADD COLUMN IF NOT EXISTS team_region    text,
    ADD COLUMN IF NOT EXISTS approved_at    timestamptz,
    ADD COLUMN IF NOT EXISTS consumed_at    timestamptz,
    ADD CONSTRAINT device_code_status_check
        CHECK (status IN ('pending', 'approved', 'denied', 'expired', 'consumed'));

CREATE INDEX IF NOT EXISTS device_code_requester_recent_idx
    ON device_code (requester_hash, created_at);
