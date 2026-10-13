-- Billing pages read usage from saved hourly rollups instead of recomputing
-- it from every interval on each request. Compute for a full past hour comes
-- from its rollup row; everything else (the current hour, bucket edges and
-- any hour without a row, whose absence proves nothing) is computed from the
-- intervals active in those windows. Storage is informational: the sum of
-- the bucket's measured hours, the current hour as of its latest rollup;
-- unknown when a saved hour has no measurement; and flagged for the caller to
-- compute directly when the bucket cuts an hour or any of its hours is unsaved.
-- Saved hours floor legacy artifact storage per hour, so a rollup-backed total
-- can sit below a direct unfloored one by under one MiB-second per hour.
CREATE OR REPLACE FUNCTION billing_usage_from_rollups(p_team uuid, p_starts timestamptz[], p_ends timestamptz[])
RETURNS TABLE(bucket_start timestamptz, bucket_end timestamptz,
              vcpu_seconds numeric, memory_mib_seconds numeric, storage_mib_seconds numeric, storage_direct boolean)
LANGUAGE sql STABLE AS $$
WITH params AS MATERIALIZED (
 -- Rollup hours are UTC hours whatever the session TimeZone.
 SELECT billing_request_now() now_, date_trunc('hour', billing_request_now(), 'UTC') cut,
  feature_enabled('billing_hourly_rollups', p_team) enabled
), buckets AS MATERIALIZED (
 SELECT s.bucket_start, e.bucket_end,
  tstzrange(s.bucket_start, GREATEST(s.bucket_start, LEAST(e.bucket_end, p.now_)), '[)') elapsed
 FROM unnest(p_starts) WITH ORDINALITY s(bucket_start, n)
 JOIN unnest(p_ends) WITH ORDINALITY e(bucket_end, n) USING (n)
 CROSS JOIN params p
), hours AS MATERIALIZED (
 -- Every hour the bucket's elapsed time touches, with its rollup row if saved.
 -- A past hour's row counts only once written after the hour ended; one
 -- captured earlier holds a partial hour until the scheduler refreshes it.
 SELECT b.bucket_start, h.hour_start, r.vcpu_seconds, r.memory_mib_seconds, r.storage_mib_seconds,
  r.team_id IS NOT NULL AND p.enabled AND (r.updated_at >= r.hour_end OR h.hour_start = p.cut) saved
 FROM buckets b CROSS JOIN params p
 CROSS JOIN LATERAL generate_series(date_trunc('hour', lower(b.elapsed), 'UTC'), upper(b.elapsed) - interval '1 microsecond', interval '1 hour') h(hour_start)
 LEFT JOIN team_billing_usage_hourly r ON r.team_id = p_team AND r.hour_start = h.hour_start
 WHERE NOT isempty(b.elapsed)
), recorded AS MATERIALIZED (
 SELECT h.* FROM hours h CROSS JOIN params p
 JOIN buckets b USING (bucket_start)
 WHERE h.saved AND h.hour_start < p.cut
   AND tstzrange(h.hour_start, h.hour_start + interval '1 hour', '[)') <@ b.elapsed
), live_windows AS MATERIALIZED (
 SELECT b.bucket_start, lower(w) ws, upper(w) we
 FROM buckets b
 CROSS JOIN LATERAL unnest(
   tstzmultirange(b.elapsed)
   - COALESCE((SELECT range_agg(tstzrange(r.hour_start, r.hour_start + interval '1 hour', '[)'))
               FROM recorded r WHERE r.bucket_start = b.bucket_start), '{}'::tstzmultirange)) w
), live_bounds AS MATERIALIZED (
 SELECT min(ws) lo, max(we) hi FROM live_windows
), live_intervals AS MATERIALIZED (
 -- Only intervals still open or ended inside the live windows can overlap
 -- them; both branches use the team's open and ended indexes.
 SELECT i.started_at, i.ended_at, i.vcpu_count, i.memory_mib
 FROM sandbox_compute_billing_interval i, live_bounds w
 WHERE i.team_id = p_team AND i.ended_at IS NULL AND i.started_at < w.hi
 UNION ALL
 SELECT i.started_at, i.ended_at, i.vcpu_count, i.memory_mib
 FROM sandbox_compute_billing_interval i, live_bounds w
 WHERE i.team_id = p_team AND i.ended_at > w.lo AND i.started_at < w.hi
), live_compute AS (
 SELECT lw.bucket_start,
  SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, p.now_), lw.we) - GREATEST(i.started_at, lw.ws))) * i.vcpu_count) vcpu,
  SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, p.now_), lw.we) - GREATEST(i.started_at, lw.ws))) * i.memory_mib) memory
 FROM live_windows lw CROSS JOIN params p
 JOIN live_intervals i ON i.started_at < lw.we AND COALESCE(i.ended_at, p.now_) > lw.ws
 GROUP BY lw.bucket_start
), storage AS (
 SELECT b.bucket_start,
  COALESCE(bool_or(h.saved AND h.storage_mib_seconds IS NULL), false) unmeasured,
  date_trunc('hour', b.bucket_start, 'UTC') <> b.bucket_start
   OR (date_trunc('hour', b.bucket_end, 'UTC') <> b.bucket_end AND b.bucket_end <= p.now_)
   OR COALESCE(bool_or(NOT h.saved), false) direct,
  COALESCE(SUM(h.storage_mib_seconds), 0) total
 FROM buckets b CROSS JOIN params p
 LEFT JOIN hours h USING (bucket_start)
 GROUP BY b.bucket_start, b.bucket_end, p.now_
)
SELECT b.bucket_start, b.bucket_end,
 COALESCE((SELECT SUM(r.vcpu_seconds) FROM recorded r WHERE r.bucket_start = b.bucket_start), 0) + COALESCE(lc.vcpu, 0),
 COALESCE((SELECT SUM(r.memory_mib_seconds) FROM recorded r WHERE r.bucket_start = b.bucket_start), 0) + COALESCE(lc.memory, 0),
 CASE WHEN s.unmeasured OR s.direct THEN NULL ELSE s.total END,
 s.direct AND NOT s.unmeasured
FROM buckets b
JOIN storage s USING (bucket_start)
LEFT JOIN live_compute lc USING (bucket_start)
ORDER BY b.bucket_start
$$;
