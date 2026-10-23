-- Which plan a team is on, for every tier. Each row is one contiguous stretch
-- with the terms and limits frozen when it began; a change closes the open row
-- and opens the next at the same instant. Paid plans point at the commercial
-- contract that bills them.
CREATE TABLE team_plan_assignment (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 team_id uuid NOT NULL REFERENCES team(id),
 plan_key text NOT NULL CHECK(btrim(plan_key)<>''),
 kind text NOT NULL CHECK(kind IN ('free','prepaid','subscription','legacy_payg','custom')),
 plan_version_id uuid REFERENCES billing_plan_version(id),
 terms jsonb NOT NULL CHECK(jsonb_typeof(terms)='object'),
 limits jsonb NOT NULL CHECK(jsonb_typeof(limits)='object' AND jsonb_typeof(limits->'max_sandboxes')='number'),
 effective_from timestamptz NOT NULL,
 effective_to timestamptz CHECK(effective_to IS NULL OR effective_to>=effective_from),
 contract_id uuid REFERENCES billing_commercial_contract(id),
 source text NOT NULL CHECK(source IN ('signup','migration','checkout','credit_purchase','plan_change','staff')),
 assigned_by uuid,
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 2000),
 idempotency_key text CHECK(idempotency_key IS NULL OR length(idempotency_key) BETWEEN 1 AND 200),
 -- The team's limits just before its first assignment, so turning pricing v2
 -- back off can restore them exactly.
 previous_limits jsonb CHECK(previous_limits IS NULL OR jsonb_typeof(previous_limits)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(team_id,idempotency_key)
);
CREATE UNIQUE INDEX team_plan_assignment_open ON team_plan_assignment(team_id) WHERE effective_to IS NULL;
CREATE INDEX team_plan_assignment_history ON team_plan_assignment(team_id,effective_from DESC);
CREATE INDEX team_plan_assignment_contract ON team_plan_assignment(contract_id) WHERE contract_id IS NOT NULL;

CREATE FUNCTION guard_team_plan_assignment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'UPDATE' AND TG_OP<>'INSERT' THEN RAISE EXCEPTION 'plan assignment history is immutable'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.effective_to IS NOT NULL OR NEW.effective_from>clock_timestamp() THEN
   RAISE EXCEPTION 'a plan assignment starts open and not in the future';
  END IF;
  IF EXISTS(SELECT 1 FROM team_plan_assignment WHERE team_id=NEW.team_id AND (effective_to IS NULL OR effective_to>NEW.effective_from)) THEN
   RAISE EXCEPTION 'plan assignments of a team cannot overlap';
  END IF;
  NEW.previous_limits:=NULL;
  IF NOT EXISTS(SELECT 1 FROM team_plan_assignment WHERE team_id=NEW.team_id) THEN
   SELECT jsonb_build_object('max_sandboxes',max_sandboxes,'max_template_vcpu',max_template_vcpu,'max_template_memory_mib',max_template_memory_mib,
    'max_template_disk_mib',max_template_disk_mib,'limits_customized',limits_customized) INTO NEW.previous_limits FROM team WHERE id=NEW.team_id;
  END IF;
  RETURN NEW;
 END IF;
 IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
  OR (to_jsonb(NEW)-'effective_to') IS DISTINCT FROM (to_jsonb(OLD)-'effective_to') THEN
  RAISE EXCEPTION 'a plan assignment only closes once';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_plan_assignment_guard BEFORE INSERT OR UPDATE OR DELETE ON team_plan_assignment
 FOR EACH ROW EXECUTE FUNCTION guard_team_plan_assignment();
CREATE TRIGGER team_plan_assignment_truncate_guard BEFORE TRUNCATE ON team_plan_assignment
 FOR EACH STATEMENT EXECUTE FUNCTION guard_team_plan_assignment();

-- Writes an assignment's frozen limits to the team, unless staff set the
-- team's limits by hand. An absent template cap falls back to the API default.
CREATE FUNCTION apply_assignment_limits(p_team uuid, p_limits jsonb) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 PERFORM set_config('superserve.applying_plan_limits','on',true);
 UPDATE team SET
  max_sandboxes=(p_limits->>'max_sandboxes')::integer,
  max_template_vcpu=(p_limits->>'max_template_vcpu')::integer,
  max_template_memory_mib=(p_limits->>'max_template_memory_mib')::integer,
  max_template_disk_mib=(p_limits->>'max_template_disk_mib')::integer
 WHERE id=p_team AND NOT limits_customized;
 PERFORM set_config('superserve.applying_plan_limits','off',true);
END $$;

-- Puts teams back on the limits they had before pricing v2 assigned them a
-- plan, for turning pricing v2 off. Teams whose limits staff have set by hand
-- since keep them. Returns how many teams were restored; run it until it
-- returns zero, as each call handles at most p_batch teams.
CREATE FUNCTION restore_pre_assignment_limits(p_batch integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE n integer;
BEGIN
 PERFORM set_config('superserve.applying_plan_limits','on',true);
 WITH first AS (
  SELECT DISTINCT ON (a.team_id) a.team_id,a.previous_limits p FROM team_plan_assignment a
  WHERE a.previous_limits IS NOT NULL ORDER BY a.team_id,a.effective_from
 ), due AS (
  SELECT t.id,f.p FROM team t JOIN first f ON f.team_id=t.id
  WHERE NOT t.limits_customized AND (t.max_sandboxes,t.max_template_vcpu,t.max_template_memory_mib,t.max_template_disk_mib)
   IS DISTINCT FROM ((f.p->>'max_sandboxes')::integer,(f.p->>'max_template_vcpu')::integer,(f.p->>'max_template_memory_mib')::integer,(f.p->>'max_template_disk_mib')::integer)
  LIMIT p_batch FOR NO KEY UPDATE OF t SKIP LOCKED
 )
 UPDATE team t SET max_sandboxes=(d.p->>'max_sandboxes')::integer,max_template_vcpu=(d.p->>'max_template_vcpu')::integer,
  max_template_memory_mib=(d.p->>'max_template_memory_mib')::integer,max_template_disk_mib=(d.p->>'max_template_disk_mib')::integer
 FROM due d WHERE t.id=d.id;
 GET DIAGNOSTICS n=ROW_COUNT;
 PERFORM set_config('superserve.applying_plan_limits','off',true);
 RETURN n;
END $$;

CREATE FUNCTION team_plan_assignment_apply_limits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM apply_assignment_limits(NEW.team_id,NEW.limits);
 RETURN NULL;
END $$;
CREATE TRIGGER team_plan_assignment_limits AFTER INSERT ON team_plan_assignment
 FOR EACH ROW EXECUTE FUNCTION team_plan_assignment_apply_limits();

ALTER TABLE team_plan_assignment ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON team_plan_assignment FROM PUBLIC;
DO $$ DECLARE r text; f regprocedure; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON team_plan_assignment FROM %I',r);
   IF r='service_role' THEN
    GRANT SELECT,INSERT,UPDATE ON team_plan_assignment TO service_role;
    CREATE POLICY team_plan_assignment_backend ON team_plan_assignment TO service_role USING(true) WITH CHECK(true);
   END IF;
  END IF;
 END LOOP;
 FOREACH f IN ARRAY ARRAY['guard_team_plan_assignment()'::regprocedure,'apply_assignment_limits(uuid,jsonb)'::regprocedure,'team_plan_assignment_apply_limits()'::regprocedure,'restore_pre_assignment_limits(integer)'::regprocedure] LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role',f); END IF;
 END LOOP;
END $$;
