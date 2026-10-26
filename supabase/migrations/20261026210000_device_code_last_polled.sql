-- When a CLI last polled its device code, so the token endpoint can answer
-- slow_down to clients that poll faster than the advertised interval.
ALTER TABLE device_code ADD COLUMN IF NOT EXISTS last_polled_at timestamptz;
