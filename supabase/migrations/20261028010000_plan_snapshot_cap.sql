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

-- Every assignment of a capped version freezes its cap, including ones written
-- in SQL that do not list it.
CREATE FUNCTION team_plan_assignment_snapshot_cap() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE cap integer;
BEGIN
 IF NEW.plan_version_id IS NOT NULL AND NOT NEW.limits ? 'max_snapshots' THEN
  SELECT max_snapshots INTO cap FROM billing_plan_version WHERE id=NEW.plan_version_id;
  IF cap IS NOT NULL THEN NEW.limits:=NEW.limits||jsonb_build_object('max_snapshots',cap); END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_plan_assignment_snapshot_cap BEFORE INSERT ON team_plan_assignment
 FOR EACH ROW EXECUTE FUNCTION team_plan_assignment_snapshot_cap();
REVOKE ALL ON FUNCTION team_plan_assignment_snapshot_cap() FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON FUNCTION team_plan_assignment_snapshot_cap() FROM %I',r); END IF;
 END LOOP;
END $$;

-- A hand-set snapshot cap is a manual override like the other caps.
CREATE OR REPLACE FUNCTION team_mark_limits_customized() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('superserve.applying_plan_limits', true) IS DISTINCT FROM 'on'
        AND (OLD.max_sandboxes, OLD.max_template_vcpu, OLD.max_template_memory_mib, OLD.max_template_disk_mib, OLD.max_snapshots)
            IS DISTINCT FROM (NEW.max_sandboxes, NEW.max_template_vcpu, NEW.max_template_memory_mib, NEW.max_template_disk_mib, NEW.max_snapshots) THEN
        NEW.limits_customized := true;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER team_limits_customized ON team;
CREATE TRIGGER team_limits_customized
    BEFORE UPDATE OF max_sandboxes, max_template_vcpu, max_template_memory_mib, max_template_disk_mib, max_snapshots ON team
    FOR EACH ROW EXECUTE FUNCTION team_mark_limits_customized();

-- Snapshot caps set by hand before plans could carry one stay as they are.
UPDATE team SET limits_customized=true WHERE NOT limits_customized AND max_snapshots<>100;
