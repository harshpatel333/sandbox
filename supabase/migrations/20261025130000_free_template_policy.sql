SELECT pricing_version FROM billing_pricing_switch WHERE singleton FOR UPDATE;
SELECT epoch FROM billing_pricing_adoption WHERE singleton FOR UPDATE;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM billing_plan_version WHERE pricing_version='v2' AND family='free' AND kind='free' AND state='published' AND max_templates=0) THEN
  RAISE EXCEPTION 'apply the canonical Free zero-template catalog migration before policy history' USING ERRCODE='55000';
 END IF;
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

CREATE OR REPLACE FUNCTION billing_free_template_policy_blocker(p_team uuid)
RETURNS text LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public,pg_temp SET row_security=off AS $$
DECLARE blocked text; pending boolean;
BEGIN
 blocked:=public.billing_pricing_adoption_blocker(p_team);
 IF blocked IS NOT NULL THEN RETURN blocked; END IF;
 IF EXISTS(SELECT 1 FROM public.billing_team_pause_target WHERE team_id=p_team AND done_at IS NULL) THEN RETURN 'unfinished_pause_target'; END IF;
 IF EXISTS(SELECT 1 FROM public.billing_free_allowance_retry WHERE team_id=p_team) THEN RETURN 'allowance_retry'; END IF;
 IF to_regclass('public.team_plan_change') IS NOT NULL THEN
  EXECUTE 'SELECT EXISTS(SELECT 1 FROM public.team_plan_change WHERE team_id=$1)' INTO pending USING p_team;
  IF pending THEN RETURN 'plan_change_owner_reconciliation'; END IF;
 END IF;
 RETURN NULL;
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
 WHERE family='free' AND pricing_version='v2' AND state='published' AND kind='free' AND max_templates=0 FOR SHARE;
 PERFORM 1 FROM public.team_billing_account WHERE team_id=p_team FOR UPDATE NOWAIT;
 PERFORM 1 FROM public.team WHERE id=p_team AND home_region=adoption.region FOR UPDATE NOWAIT;
 IF NOT FOUND THEN RAISE EXCEPTION 'team missing or outside prepared region' USING ERRCODE='55000'; END IF;
 PERFORM 1 FROM public.team_billing_account WHERE team_id=p_team FOR UPDATE NOWAIT;
 SELECT * INTO old_assignment FROM public.team_plan_assignment WHERE team_id=p_team AND effective_to IS NULL FOR UPDATE NOWAIT;
 IF NOT FOUND OR old_assignment.id<>p_expected_assignment OR old_assignment.kind<>'free' OR old_assignment.contract_id IS NOT NULL THEN
  RAISE EXCEPTION 'current Free assignment identity changed' USING ERRCODE='55000';
 END IF;
 IF old_assignment.limits->'max_templates'='0'::jsonb THEN RETURN old_assignment.id; END IF;
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
  RAISE EXCEPTION 'Free template enforcement migration must complete before release' USING ERRCODE='55000';
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
 REVOKE ALL ON FUNCTION billing_migrate_free_template_assignment(text,uuid,uuid,uuid,text,text),billing_free_template_policy_blocker(uuid),billing_free_template_policy_pending_targets(),guard_free_template_policy_release() FROM PUBLIC;
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON billing_free_template_policy_transition FROM %I',r);
   EXECUTE format('REVOKE ALL ON FUNCTION billing_migrate_free_template_assignment(text,uuid,uuid,uuid,text,text),billing_free_template_policy_blocker(uuid),billing_free_template_policy_pending_targets(),guard_free_template_policy_release() FROM %I',r);
  END IF;
 END LOOP;
END $$;

CREATE OR REPLACE FUNCTION billing_pricing_adoption_evidence_valid(p_region text,p_system uuid,p_epoch uuid,p_evidence jsonb)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT false; $$;
