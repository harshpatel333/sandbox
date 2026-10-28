-- A plan can cap a team's total snapshots. The cap is internal, like the
-- template cap, and is frozen into assignment limits like the other caps.
ALTER TABLE billing_plan_version ADD COLUMN max_snapshots integer CHECK (max_snapshots > 0);

DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT UPDATE (max_snapshots) ON billing_plan_version TO service_role;
 END IF;
END $$;

-- A plan without a snapshot cap leaves the team's current cap in place.
CREATE OR REPLACE FUNCTION apply_assignment_limits(p_team uuid, p_limits jsonb) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 PERFORM set_config('superserve.applying_plan_limits','on',true);
 PERFORM set_config('superserve.applying_assignment_limits','on',true);
 UPDATE team SET
  max_sandboxes=(p_limits->>'max_concurrent')::integer,
  max_template_vcpu=(p_limits->>'max_vcpu')::integer,
  max_template_memory_mib=(p_limits->>'max_memory_mib')::integer,
  max_template_disk_mib=(p_limits->>'max_disk_mib')::integer,
  max_snapshots=COALESCE((p_limits->>'max_snapshots')::integer,max_snapshots)
 WHERE id=p_team AND NOT limits_customized;
 PERFORM set_config('superserve.applying_assignment_limits','off',true);
 PERFORM set_config('superserve.applying_plan_limits','off',true);
END $$;
