-- Hours whose storage is not billable record compute without waiting for the
-- storage calculation; their storage stays NULL until it is measured, never 0.
ALTER TABLE team_billing_usage_hourly ALTER COLUMN storage_mib_seconds DROP NOT NULL;
