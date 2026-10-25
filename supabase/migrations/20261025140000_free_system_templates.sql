SELECT pricing_version FROM billing_pricing_switch WHERE singleton FOR UPDATE;
SELECT epoch FROM billing_pricing_adoption WHERE singleton FOR UPDATE;
ALTER TABLE sandbox ADD COLUMN system_template boolean NOT NULL DEFAULT false;
ALTER TABLE sandbox_snapshot ADD COLUMN system_template boolean NOT NULL DEFAULT false;

DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM sandbox s JOIN template t ON t.id=s.template_id
  LEFT JOIN billing_pricing_adoption a ON a.singleton
  WHERE t.team_id<>s.team_id AND t.team_id IS DISTINCT FROM a.system_team_id) THEN
  RAISE EXCEPTION 'prepare the canonical system team and reconcile unknown foreign template owners before policy migration' USING ERRCODE='55000';
 END IF;
END $$;
UPDATE sandbox s SET system_template = true
FROM template t,billing_pricing_adoption a WHERE a.singleton AND t.id=s.template_id AND t.team_id=a.system_team_id;

UPDATE sandbox_snapshot ss SET system_template = s.system_template
FROM sandbox s WHERE s.id = ss.sandbox_id AND s.team_id = ss.team_id;

CREATE OR REPLACE FUNCTION guard_free_sandbox_shape() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f record;
BEGIN
 BEGIN
  PERFORM 1 FROM team WHERE id=NEW.team_id FOR SHARE NOWAIT;
 EXCEPTION WHEN lock_not_available THEN
  RAISE EXCEPTION 'team plan assignment is changing' USING ERRCODE='SS003';
 END;
 SELECT * INTO f FROM billing_free_plan(NEW.team_id);
 IF f.free_since IS NOT NULL AND ((f.max_vcpu IS NOT NULL AND NEW.vcpu_count>f.max_vcpu)
  OR (f.max_memory_mib IS NOT NULL AND NEW.memory_mib>f.max_memory_mib)
  OR (f.max_disk_mib IS NOT NULL AND NEW.disk_mib>f.max_disk_mib)) THEN
  RAISE EXCEPTION 'sandbox exceeds plan limits' USING ERRCODE='SS002',
   DETAIL=jsonb_build_object('max_vcpu',COALESCE(f.max_vcpu,0),'max_memory_mib',COALESCE(f.max_memory_mib,0),
    'max_disk_mib',COALESCE(f.max_disk_mib,0),'vcpu',NEW.vcpu_count,'memory_mib',NEW.memory_mib,'disk_mib',NEW.disk_mib)::text;
 END IF;
 IF f.free_since IS NOT NULL AND TG_OP = 'INSERT' AND NOT NEW.system_template THEN
  RAISE EXCEPTION 'Free supports only default and system templates' USING ERRCODE='SS004';
 END IF;
 RETURN NEW;
END $$;


CREATE OR REPLACE FUNCTION team_effective_limits(p_team uuid)
RETURNS TABLE(max_concurrent integer, max_vcpu integer, max_memory_mib integer, max_disk_mib integer, max_templates integer)
LANGUAGE sql STABLE AS $$
    WITH base AS (
        SELECT max_template_vcpu, max_template_memory_mib, max_template_disk_mib, max_templates
        FROM billing_plan_version WHERE family = 'payg' AND pricing_version = 'legacy' AND kind = 'legacy_payg' AND state <> 'draft'
        ORDER BY state = 'published' DESC, version DESC LIMIT 1
    ), assigned AS (
        SELECT kind, (limits->>'max_templates')::integer AS max_templates FROM team_plan_assignment
        WHERE team_id=p_team AND effective_to IS NULL
    ), frozen AS (
        SELECT l.max_templates FROM billing_commercial_contract c JOIN billing_contract_limits l ON l.contract_id = c.id
        WHERE c.team_id = p_team AND c.status = 'active' ORDER BY c.starts_at DESC LIMIT 1
    )
    SELECT t.max_sandboxes,
        COALESCE(t.max_template_vcpu, b.max_template_vcpu),
        COALESCE(t.max_template_memory_mib, b.max_template_memory_mib),
        COALESCE(t.max_template_disk_mib, b.max_template_disk_mib),
        CASE WHEN (SELECT kind FROM assigned)='free' THEN 0 ELSE COALESCE(t.max_templates, (SELECT max_templates FROM assigned), (SELECT max_templates FROM frozen), b.max_templates) END
    FROM team t LEFT JOIN base b ON true WHERE t.id = p_team;
$$;


