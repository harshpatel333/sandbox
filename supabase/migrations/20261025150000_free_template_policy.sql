BEGIN;

SELECT pricing_version FROM billing_pricing_switch WHERE singleton FOR UPDATE;
SELECT epoch FROM billing_pricing_adoption WHERE singleton FOR UPDATE;
LOCK TABLE billing_plan_version, plan_limits, billing_contract_limits IN ACCESS EXCLUSIVE MODE NOWAIT;
DO $$ BEGIN
    IF NOT pg_try_advisory_xact_lock(hashtext('billing-catalog:free:v2')::bigint) THEN
        RAISE EXCEPTION 'Free catalog is busy; retry the migration' USING ERRCODE = 'lock_not_available';
    END IF;
END $$;

ALTER TABLE billing_plan_version
    DROP CONSTRAINT billing_plan_version_max_templates_check,
    ADD CONSTRAINT billing_plan_version_max_templates_check CHECK (max_templates >= 0);
ALTER TABLE plan_limits
    DROP CONSTRAINT plan_limits_max_templates_check,
    ADD CONSTRAINT plan_limits_max_templates_check CHECK (max_templates >= 0);
ALTER TABLE billing_contract_limits
    DROP CONSTRAINT billing_contract_limits_max_templates_check,
    ADD CONSTRAINT billing_contract_limits_max_templates_check CHECK (max_templates >= 0);

DO $$
DECLARE
    definition text := pg_get_functiondef('public.publish_billing_plan_version(uuid,uuid)'::regprocedure);
BEGIN
    IF position('n.state IN (''published'', ''retired'') AND n.version > d.version' IN definition) > 0 THEN
        RETURN;
    END IF;
    IF position('n.state = ''published'' AND n.version > d.version' IN definition) = 0 THEN
        RAISE EXCEPTION 'unexpected catalog publication guard' USING ERRCODE = '55000';
    END IF;
    EXECUTE replace(definition,
        'n.state = ''published'' AND n.version > d.version',
        'n.state IN (''published'', ''retired'') AND n.version > d.version');
END $$;

DO $$
DECLARE
    predecessor billing_plan_version;
    successor billing_plan_version;
BEGIN
    SELECT * INTO STRICT predecessor FROM billing_plan_version
    WHERE pricing_version = 'v2' AND family = 'free' AND kind = 'free' AND state = 'published'
    FOR UPDATE;
    IF predecessor.id = md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid AND predecessor.max_templates = 0 THEN
        RETURN;
    END IF;
    IF predecessor.id IS DISTINCT FROM md5('billing-plan-version:v2:free:1')::uuid OR predecessor.max_templates IS DISTINCT FROM 10 THEN
        RAISE EXCEPTION 'unexpected Free catalog predecessor' USING ERRCODE = '55000';
    END IF;

    successor := predecessor;
    successor.id := md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid;
    SELECT max(version) + 1 INTO successor.version FROM billing_plan_version WHERE family = 'free';
    successor.state := 'draft';
    successor.max_templates := 0;
    successor.created_by := NULL;
    successor.created_at := clock_timestamp();
    successor.updated_at := successor.created_at;
    successor.published_by := NULL;
    successor.published_at := NULL;
    successor.retired_by := NULL;
    successor.retired_at := NULL;

    INSERT INTO billing_plan_version SELECT successor.*;
    PERFORM publish_billing_plan_version(successor.id, NULL);
END $$;

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


ALTER TABLE team_plan_assignment ADD COLUMN free_allowance_origin_at timestamptz
 CHECK(free_allowance_origin_at IS NULL OR (kind='free' AND free_allowance_origin_at<=effective_from));

CREATE OR REPLACE FUNCTION billing_free_plan(p_team uuid)
RETURNS TABLE(free_since timestamptz, allowance jsonb, max_vcpu int, max_memory_mib int, max_disk_mib int)
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(a.free_allowance_origin_at,a.effective_from),
        CASE WHEN jsonb_typeof(a.terms->'allowance') = 'object' THEN a.terms->'allowance' ELSE '{}'::jsonb END,
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_vcpu END, billing_json_cap(a.limits->'max_vcpu')),
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_memory_mib END, billing_json_cap(a.limits->'max_memory_mib')),
        COALESCE(CASE WHEN t.limits_customized THEN t.max_template_disk_mib END, billing_json_cap(a.limits->'max_disk_mib'))
 FROM team_plan_assignment a JOIN team t ON t.id = a.team_id
 WHERE a.team_id = p_team AND a.effective_to IS NULL AND a.kind = 'free';
$$;

CREATE TABLE billing_free_template_policy_transition (
 predecessor_id uuid PRIMARY KEY REFERENCES team_plan_assignment(id),
 successor_id uuid NOT NULL UNIQUE REFERENCES team_plan_assignment(id),
 team_id uuid NOT NULL,
 epoch uuid NOT NULL,
 actor text NOT NULL CHECK(length(btrim(actor)) BETWEEN 1 AND 2000),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 2000),
 prior_credit_resolution jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE billing_free_template_policy_transition ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_free_template_policy_transition FROM PUBLIC;
CREATE TRIGGER billing_free_template_policy_transition_immutable BEFORE UPDATE OR DELETE OR TRUNCATE
 ON billing_free_template_policy_transition FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_append_only();

CREATE FUNCTION billing_free_template_policy_assignment_ready(p_assignment team_plan_assignment)
RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
 WITH previous AS (
  SELECT a.* FROM public.team_plan_assignment a
  WHERE a.team_id=(p_assignment).team_id AND a.effective_to<=(p_assignment).effective_from
  ORDER BY a.effective_from DESC LIMIT 1
 )
 SELECT COALESCE((p_assignment).kind='free' AND (p_assignment).plan_key='free' AND (p_assignment).effective_to IS NULL
  AND (p_assignment).contract_id IS NULL
  AND (p_assignment).plan_version_id=md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid
  AND (p_assignment).limits->'max_templates'='0'::jsonb
  AND CASE WHEN NOT EXISTS(SELECT 1 FROM previous WHERE kind='free') THEN
   (p_assignment).free_allowance_origin_at IS NULL
  ELSE EXISTS(
   SELECT 1 FROM previous p JOIN public.billing_free_template_policy_transition r
    ON r.predecessor_id=p.id AND r.successor_id=(p_assignment).id AND r.team_id=(p_assignment).team_id
   WHERE p.plan_version_id=md5('billing-plan-version:v2:free:1')::uuid AND p.limits->'max_templates'='10'::jsonb
    AND p.effective_to=(p_assignment).effective_from
    AND (p_assignment).free_allowance_origin_at=COALESCE(p.free_allowance_origin_at,p.effective_from)
    AND ((p_assignment).terms-'version_id'-'version')=(p.terms-'version_id'-'version')
    AND ((p_assignment).limits-'max_templates')=(p.limits-'max_templates')
  ) END,false);
$$;

CREATE OR REPLACE FUNCTION billing_free_template_policy_blocker(p_team uuid)
RETURNS text LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
DECLARE blocked text; pending boolean;
BEGIN
 blocked:=public.billing_pricing_adoption_blocker(p_team);
 IF blocked IS NOT NULL THEN RETURN blocked; END IF;
 IF EXISTS(SELECT 1 FROM public.billing_team_pause_target t WHERE t.team_id=p_team AND t.done_at IS NULL AND NOT public.billing_pause_target_settled(t)) THEN RETURN 'unfinished_pause_target'; END IF;
 IF EXISTS(SELECT 1 FROM public.billing_free_allowance_retry WHERE team_id=p_team) THEN RETURN 'allowance_retry'; END IF;
 IF to_regclass('public.team_plan_change') IS NOT NULL THEN
  EXECUTE 'SELECT EXISTS(SELECT 1 FROM public.team_plan_change WHERE team_id=$1)' INTO pending USING p_team;
  IF pending THEN RETURN 'plan_change_owner_reconciliation'; END IF;
 END IF;
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION request_team_pause_all(p_team uuid, p_reason text)
RETURNS integer LANGUAGE plpgsql AS $$
DECLARE targets integer;
BEGIN
 PERFORM 1 FROM team WHERE id=p_team FOR SHARE;
 WITH requested AS (
  INSERT INTO billing_team_pause_request(assignment_id,team_id)
  SELECT id,team_id FROM team_plan_assignment
  WHERE team_id=p_team AND effective_to IS NULL AND kind='free'
  ON CONFLICT DO NOTHING
  RETURNING assignment_id,team_id,requested_at
 ), added AS (
  INSERT INTO billing_team_pause_target (sandbox_id, team_id, assignment_id, reason, requested_at)
  SELECT s.id, s.team_id, r.assignment_id, p_reason, r.requested_at
  FROM requested r JOIN sandbox s ON s.team_id=r.team_id
  WHERE s.destroyed_at IS NULL AND s.status NOT IN ('paused', 'failed', 'deleted')
  RETURNING 1
 )
 SELECT count(*)::int INTO targets FROM added;
 RETURN targets;
END $$;

CREATE OR REPLACE FUNCTION billing_migrate_free_template_assignment(p_region text,p_system uuid,p_team uuid,p_expected_assignment uuid,p_actor text,p_reason text)
RETURNS uuid LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
DECLARE
 old_assignment public.team_plan_assignment;
 successor uuid;
 v public.billing_plan_version;
 adoption public.billing_pricing_adoption;
 credit public.billing_pricing_adoption_credit_resolution;
 raw text;
 blocked text;
 boundary timestamptz;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'Free policy transition requires READ COMMITTED' USING ERRCODE='55000';
 END IF;
 IF p_region IS NULL OR p_region NOT IN ('use','usw') OR p_system IS NULL OR p_system='00000000-0000-0000-0000-000000000000' OR p_team IS NULL OR p_expected_assignment IS NULL OR COALESCE(length(btrim(p_actor)),0) NOT BETWEEN 1 AND 2000
  OR COALESCE(length(btrim(p_reason)),0) NOT BETWEEN 1 AND 2000 THEN
  RAISE EXCEPTION 'team, expected assignment, actor and reason required' USING ERRCODE='22023';
 END IF;
 PERFORM * FROM public.billing_lock_pricing_public_state();
 SELECT * INTO adoption FROM public.billing_pricing_adoption WHERE singleton;
 IF adoption.region IS DISTINCT FROM p_region OR adoption.system_team_id IS DISTINCT FROM p_system THEN
  RAISE EXCEPTION 'Free policy region/system binding changed' USING ERRCODE='55000';
 END IF;
 SELECT r.successor_id INTO successor FROM public.billing_free_template_policy_transition r
 WHERE r.predecessor_id=p_expected_assignment AND r.team_id=p_team;
 IF FOUND THEN RETURN successor; END IF;
 SELECT s.pricing_version INTO raw FROM public.billing_pricing_switch s WHERE singleton;
 
 IF raw IS DISTINCT FROM 'v2' OR adoption.state IS DISTINCT FROM 'preparing' OR adoption.region IS NULL
  OR adoption.system_team_id IS NULL OR p_team=adoption.system_team_id THEN
  RAISE EXCEPTION 'Free policy transition requires prepared raw v2' USING ERRCODE='55000';
 END IF;
 SELECT * INTO STRICT v FROM public.billing_plan_version
 WHERE id=md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid AND family='free' AND pricing_version='v2' AND state='published' AND kind='free' AND max_templates=0 FOR SHARE;
 PERFORM 1 FROM public.team_billing_account WHERE team_id=p_team FOR UPDATE NOWAIT;
 PERFORM 1 FROM public.team WHERE id=p_team AND home_region=adoption.region FOR UPDATE NOWAIT;
 IF NOT FOUND THEN RAISE EXCEPTION 'team missing or outside prepared region' USING ERRCODE='55000'; END IF;
 PERFORM 1 FROM public.team_billing_account WHERE team_id=p_team FOR UPDATE NOWAIT;
 SELECT * INTO old_assignment FROM public.team_plan_assignment WHERE team_id=p_team AND effective_to IS NULL FOR UPDATE NOWAIT;
 IF NOT FOUND OR old_assignment.id<>p_expected_assignment OR old_assignment.kind<>'free' OR old_assignment.contract_id IS NOT NULL THEN
  RAISE EXCEPTION 'current Free assignment identity changed' USING ERRCODE='55000';
 END IF;
 IF old_assignment.plan_version_id IS DISTINCT FROM md5('billing-plan-version:v2:free:1')::uuid
  OR old_assignment.plan_key IS DISTINCT FROM 'free' OR old_assignment.limits->'max_templates' IS DISTINCT FROM '10'::jsonb THEN
  RAISE EXCEPTION 'ineligible Free predecessor version' USING ERRCODE='55000';
 END IF;
 blocked:=public.billing_free_template_policy_blocker(p_team);
 IF blocked IS NOT NULL THEN RAISE EXCEPTION 'unresolved authority: %',blocked USING ERRCODE='55000'; END IF;
 SELECT * INTO credit FROM public.billing_pricing_adoption_credit_resolution WHERE team_id=p_team FOR UPDATE;
 boundary:=clock_timestamp();
 UPDATE public.team_plan_assignment SET effective_to=boundary WHERE id=old_assignment.id;
 INSERT INTO public.team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,contract_id,source,assigned_by,reason,idempotency_key,free_allowance_origin_at)
 VALUES(p_team,old_assignment.plan_key,'free',v.id,
  old_assignment.terms||jsonb_build_object('version_id',v.id,'version',v.version),
  old_assignment.limits||'{"max_templates":0}'::jsonb,boundary,NULL,'migration',NULL,p_reason,
  'free-template-policy:'||old_assignment.id::text,COALESCE(old_assignment.free_allowance_origin_at,old_assignment.effective_from))
 RETURNING id INTO successor;
 INSERT INTO public.billing_free_template_policy_transition(predecessor_id,successor_id,team_id,epoch,actor,reason,prior_credit_resolution)
 VALUES(old_assignment.id,successor,p_team,adoption.epoch,p_actor,p_reason,CASE WHEN credit.team_id IS NOT NULL THEN to_jsonb(credit) END);
 UPDATE public.billing_pricing_adoption_credit_resolution SET assignment_id=successor
 WHERE team_id=p_team AND assignment_id=old_assignment.id AND source=public.billing_pricing_adoption_credit_source(p_team);
 INSERT INTO public.billing_pricing_adoption_event(epoch,region,action,actor,reason,details)
 VALUES(adoption.epoch,adoption.region,'free_template_policy_assignment',p_actor,p_reason,
  jsonb_build_object('team_id',p_team,'predecessor_id',old_assignment.id,'successor_id',successor,'free_allowance_origin_at',COALESCE(old_assignment.free_allowance_origin_at,old_assignment.effective_from)));
 RETURN successor;
END $$;

CREATE OR REPLACE FUNCTION billing_free_template_policy_pending_targets()
RETURNS jsonb LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
DECLARE pending jsonb;
BEGIN
 IF to_regclass('public.team_plan_change') IS NULL THEN RETURN '[]'::jsonb; END IF;
 EXECUTE 'SELECT COALESCE(jsonb_agg(jsonb_build_object(''team_id'',team_id,''state'',state,''target_limits'',target_limits)),''[]''::jsonb) FROM public.team_plan_change WHERE state IN (''scheduled'',''applied'') AND target_kind=''free'' AND target_limits->''max_templates'' IS DISTINCT FROM ''0''::jsonb' INTO pending;
 RETURN pending;
END $$;

CREATE OR REPLACE FUNCTION guard_free_template_policy_release() RETURNS trigger LANGUAGE plpgsql
 SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
BEGIN
 IF NEW.state='released' THEN
  IF jsonb_array_length(public.billing_free_template_policy_pending_targets())<>0 THEN
   RAISE EXCEPTION 'committed old-Free plan changes require owner reconciliation' USING ERRCODE='55000';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.billing_plan_version WHERE id=md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid AND family='free' AND pricing_version='v2' AND kind='free' AND state='published' AND max_templates=0)
   OR EXISTS(SELECT 1 FROM public.team_plan_assignment a JOIN public.team t ON t.id=a.team_id
    WHERE a.effective_to IS NULL AND a.kind='free' AND a.team_id<>NEW.system_team_id
     AND NOT public.billing_free_template_policy_assignment_ready(a)) THEN
   RAISE EXCEPTION 'Free0 catalog and fenced existing-assignment policy completion required' USING ERRCODE='55000';
  END IF;
  IF COALESCE(NEW.evidence->'free_template_policy'->'existing_assignments_verified','false'::jsonb)<>'true'::jsonb
   OR COALESCE(btrim(NEW.evidence->'free_template_policy'->>'custom_policy_reference'),'')=''
   OR COALESCE(btrim(NEW.evidence->'free_template_policy'->>'runtime_reference'),'')='' THEN
   RAISE EXCEPTION 'explicit Free policy, runtime and current-assignment evidence required' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_free_template_policy_release BEFORE UPDATE ON billing_pricing_adoption
 FOR EACH ROW EXECUTE FUNCTION guard_free_template_policy_release();

WITH reset AS (
 UPDATE billing_pricing_adoption SET epoch=gen_random_uuid(),state='preparing',evidence=NULL,released_at=NULL
 WHERE singleton RETURNING epoch,region
)
INSERT INTO billing_pricing_adoption_event(epoch,region,action,actor,reason,details)
SELECT epoch,region,'free_template_policy_reset','database','Free template policy requires fresh assembled evidence','{}'::jsonb FROM reset;

DO $$ DECLARE r text; BEGIN
 REVOKE ALL ON FUNCTION billing_migrate_free_template_assignment(text,uuid,uuid,uuid,text,text),billing_free_template_policy_blocker(uuid),billing_free_template_policy_pending_targets(),billing_free_template_policy_assignment_ready(team_plan_assignment),guard_free_template_policy_release() FROM PUBLIC;
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON billing_free_template_policy_transition FROM %I',r);
   EXECUTE format('REVOKE ALL ON FUNCTION billing_migrate_free_template_assignment(text,uuid,uuid,uuid,text,text),billing_free_template_policy_blocker(uuid),billing_free_template_policy_pending_targets(),billing_free_template_policy_assignment_ready(team_plan_assignment),guard_free_template_policy_release() FROM %I',r);
  END IF;
 END LOOP;
END $$;

CREATE OR REPLACE FUNCTION billing_pricing_adoption_evidence_valid(p_region text,p_system uuid,p_epoch uuid,p_evidence jsonb)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT COALESCE(p_region IN ('use','usw') AND p_system<>'00000000-0000-0000-0000-000000000000' AND p_epoch IS NOT NULL
  AND jsonb_typeof(p_evidence)='object'
  AND p_evidence->>'region'=p_region AND p_evidence->>'system_team_id'=p_system::text AND p_evidence->>'epoch'=p_epoch::text
  AND p_evidence->>'assembled_sha' ~ '^[0-9a-f]{40}$'
  AND p_evidence->>'worker_sha'=p_evidence->>'assembled_sha' AND p_evidence->>'schema_sha'=p_evidence->>'assembled_sha'
  AND btrim(p_evidence->>'deployment_reference')<>'' AND btrim(p_evidence->>'worker_reference')<>''
  AND p_evidence->'verified_worker'='true'::jsonb
  AND p_evidence->'free_template_policy'->'existing_assignments_verified'='true'::jsonb
  AND btrim(p_evidence->'free_template_policy'->>'custom_policy_reference')<>''
  AND btrim(p_evidence->'free_template_policy'->>'runtime_reference')<>'',false);
$$;

COMMIT;
