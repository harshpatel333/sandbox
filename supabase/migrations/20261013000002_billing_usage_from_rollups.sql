-- Billing pages read usage from saved hourly rollups instead of recomputing
-- it from every interval on each request. A full past hour comes from its
-- rollup row, or is empty when the scheduler has provably had the chance to
-- enqueue it and did not; everything else (the current hour, bucket edges,
-- hours still awaiting a rollup, time before rollup coverage) is computed
-- from the intervals active in those windows. Storage is informational: the
-- sum of measured hours, unknown when a settled hour has no measurement.
CREATE OR REPLACE FUNCTION billing_usage_from_rollups(p_team uuid, p_starts timestamptz[], p_ends timestamptz[], p_floor_legacy_artifacts boolean DEFAULT false)
RETURNS TABLE(bucket_start timestamptz, bucket_end timestamptz,
              vcpu_seconds numeric, memory_mib_seconds numeric, storage_mib_seconds numeric)
LANGUAGE sql STABLE AS $$
WITH params AS MATERIALIZED (
 SELECT billing_request_now() now_,
  date_trunc('hour', billing_request_now()) cut,
  -- An hour with intervals is enqueued within minutes of ending, by the
  -- rolling window for the last day and by the backfill cursor before it.
  date_trunc('hour', billing_request_now()) - interval '1 hour' settled,
  date_trunc('hour', billing_request_now()) - interval '23 hours' rolling_from,
  (SELECT next_hour_start FROM billing_rollup_team_backfill_state WHERE team_id = p_team) backfilled_until,
  CASE WHEN feature_enabled('billing_hourly_rollups', p_team)
       THEN (SELECT min(hour_start) FROM team_billing_usage_hourly WHERE team_id = p_team) END covered_from
), buckets AS MATERIALIZED (
 SELECT s.bucket_start, e.bucket_end
 FROM unnest(p_starts) WITH ORDINALITY s(bucket_start, n)
 JOIN unnest(p_ends) WITH ORDINALITY e(bucket_end, n) USING (n)
), hours AS MATERIALIZED (
 SELECT b.bucket_start, h.hour_start
 FROM buckets b CROSS JOIN params p
 CROSS JOIN LATERAL generate_series(
   date_trunc('hour', b.bucket_start)
     + CASE WHEN date_trunc('hour', b.bucket_start) < b.bucket_start THEN interval '1 hour' ELSE interval '0' END,
   LEAST(b.bucket_end, p.cut) - interval '1 hour', interval '1 hour') h(hour_start)
 WHERE h.hour_start >= p.covered_from
), classified AS MATERIALIZED (
 SELECT x.bucket_start, x.hour_start, r.vcpu_seconds, r.memory_mib_seconds,
  CASE WHEN r.team_id IS NOT NULL THEN 'recorded'
       WHEN j.team_id IS NULL AND x.hour_start < p.settled
        AND (x.hour_start >= p.rolling_from OR x.hour_start < p.backfilled_until) THEN 'empty'
       ELSE 'live' END source
 FROM hours x CROSS JOIN params p
 LEFT JOIN team_billing_usage_hourly r ON r.team_id = p_team AND r.hour_start = x.hour_start
 LEFT JOIN billing_rollup_job j ON j.team_id = p_team AND j.hour_start = x.hour_start
), live_windows AS MATERIALIZED (
 SELECT b.bucket_start, lower(w) ws, upper(w) we
 FROM buckets b CROSS JOIN params p
 CROSS JOIN LATERAL unnest(
   tstzmultirange(tstzrange(b.bucket_start, GREATEST(b.bucket_start, LEAST(b.bucket_end, p.now_)), '[)'))
   - COALESCE((SELECT range_agg(tstzrange(c.hour_start, c.hour_start + interval '1 hour', '[)'))
               FROM classified c WHERE c.bucket_start = b.bucket_start AND c.source <> 'live'),
              '{}'::tstzmultirange)) w
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
  -- A bucket starting in the hour still in progress has nothing saved yet.
  -- A team without rollups keeps the direct calculation. A bucket starting
  -- in the hour still in progress has nothing saved yet.
  CASE WHEN p.covered_from IS NULL THEN storage_mib_seconds(p_team, b.bucket_start, b.bucket_end, p_floor_legacy_artifacts)
       WHEN b.bucket_start < p.covered_from
         OR (b.bucket_start >= p.cut AND b.bucket_start < p.now_)
         OR EXISTS (SELECT 1 FROM classified c WHERE c.bucket_start = b.bucket_start
                    AND c.source = 'live' AND c.hour_start < p.settled)
         OR EXISTS (SELECT 1 FROM team_billing_usage_hourly r
                    WHERE r.team_id = p_team AND r.hour_start >= date_trunc('hour', b.bucket_start)
                      AND r.hour_start < LEAST(b.bucket_end, p.settled) AND r.storage_mib_seconds IS NULL)
       THEN NULL
       ELSE COALESCE((
         SELECT SUM(r.storage_mib_seconds
                    * EXTRACT(EPOCH FROM (LEAST(r.hour_end, b.bucket_end) - GREATEST(r.hour_start, b.bucket_start))) / 3600)
         FROM team_billing_usage_hourly r
         WHERE r.team_id = p_team AND r.hour_start >= date_trunc('hour', b.bucket_start)
           AND r.hour_start < LEAST(b.bucket_end, p.cut) AND r.storage_mib_seconds IS NOT NULL), 0)
  END storage_mib_seconds
 FROM buckets b CROSS JOIN params p
)
SELECT b.bucket_start, b.bucket_end,
 COALESCE((SELECT SUM(c.vcpu_seconds) FROM classified c WHERE c.bucket_start = b.bucket_start AND c.source = 'recorded'), 0)
   + COALESCE(lc.vcpu, 0),
 COALESCE((SELECT SUM(c.memory_mib_seconds) FROM classified c WHERE c.bucket_start = b.bucket_start AND c.source = 'recorded'), 0)
   + COALESCE(lc.memory, 0),
 s.storage_mib_seconds
FROM buckets b
JOIN storage s USING (bucket_start)
LEFT JOIN live_compute lc USING (bucket_start)
ORDER BY b.bucket_start
$$;
