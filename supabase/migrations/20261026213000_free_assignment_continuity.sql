-- A template-cap correction must not restart an uninterrupted Free allowance.
-- Stop at a gap, a paid assignment, or a changed allowance. Strictly decreasing
-- timestamps also bound traversal of zero-length or malformed history.
CREATE OR REPLACE FUNCTION billing_free_plan(p_team uuid)
RETURNS TABLE(free_since timestamptz, allowance jsonb, max_vcpu int, max_memory_mib int, max_disk_mib int)
LANGUAGE sql STABLE AS $$
 WITH RECURSIVE continuous AS (
  SELECT a.id,a.effective_from,a.terms FROM team_plan_assignment a
  WHERE a.team_id=p_team AND a.effective_to IS NULL AND a.kind='free'
  UNION ALL
  SELECT p.id,p.effective_from,p.terms
  FROM continuous c JOIN LATERAL (
   SELECT h.* FROM team_plan_assignment h
   WHERE h.team_id=p_team AND h.effective_to=c.effective_from
   ORDER BY h.effective_from DESC,h.created_at DESC,h.id LIMIT 1
  ) p ON p.effective_from<c.effective_from AND p.kind='free'
   AND p.terms->'allowance' IS NOT DISTINCT FROM c.terms->'allowance'
 )
 SELECT (SELECT min(effective_from) FROM continuous),
        CASE WHEN jsonb_typeof(a.terms->'allowance')='object' THEN a.terms->'allowance' ELSE '{}'::jsonb END,
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_vcpu END,billing_json_cap(a.limits->'max_vcpu')),
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_memory_mib END,billing_json_cap(a.limits->'max_memory_mib')),
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_disk_mib END,billing_json_cap(a.limits->'max_disk_mib'))
 FROM team_plan_assignment a JOIN team t ON t.id=a.team_id
 WHERE a.team_id=p_team AND a.effective_to IS NULL AND a.kind='free';
$$;
