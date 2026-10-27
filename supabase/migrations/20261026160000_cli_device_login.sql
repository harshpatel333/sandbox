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

-- Starts a device login unless the requester already started p_max_per_window
-- codes in the window. The per-requester advisory lock makes the count and the
-- insert one step, so a burst of concurrent starts can't all pass the limit.
CREATE OR REPLACE FUNCTION public.start_device_code(
    p_device_code      text,
    p_user_code        text,
    p_client_name      text,
    p_requester_hash   text,
    p_expires_at       timestamptz,
    p_max_per_window   integer,
    p_window           interval
) RETURNS boolean LANGUAGE plpgsql AS $$
BEGIN
    -- A NULL in any of these would skip the lock and the count.
    IF p_requester_hash IS NULL OR p_max_per_window IS NULL OR p_window IS NULL THEN
        RAISE EXCEPTION 'invalid device code request' USING ERRCODE = '22023';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtext('device_code'), hashtext(p_requester_hash));
    IF (SELECT count(*) FROM public.device_code
        WHERE requester_hash = p_requester_hash
          AND created_at > now() - p_window) >= p_max_per_window THEN
        RETURN false;
    END IF;
    INSERT INTO public.device_code
        (device_code, user_code, client_name, requester_hash, status, expires_at)
    VALUES
        (p_device_code, p_user_code, p_client_name, p_requester_hash, 'pending', p_expires_at);
    RETURN true;
END $$;

REVOKE ALL ON FUNCTION public.start_device_code(text, text, text, text, timestamptz, integer, interval) FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON FUNCTION public.start_device_code(text, text, text, text, timestamptz, integer, interval) FROM %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
        GRANT EXECUTE ON FUNCTION public.start_device_code(text, text, text, text, timestamptz, integer, interval) TO service_role;
    END IF;
END $$;
