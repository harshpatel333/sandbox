-- Every curated template now ships at 2 vCPU / 4096 MiB memory / 8192 MiB
-- disk. A sandbox inherits its template's shape and admission rejects a shape
-- above the team's plan caps, so the plans that sat below that shape move up
-- to it. Published catalog versions are immutable: each change is a new
-- published version that copies every other term from its predecessor.
--
--  1. v2 Free: new teams freeze its caps at signup.
--  2. Legacy pay-as-you-go: legacy teams without caps of their own fall back
--     to its latest published version (team_effective_limits).
--  3. Open Free and legacy pay-as-you-go assignments: existing teams carry
--     frozen caps, so each one still below the shape closes and reopens on the
--     new version of its plan (billing_raise_plan_shape). A team with a
--     purchase in flight keeps its assignment, since the checkout names it;
--     the Free allowance sweep calls the same function so such a team moves
--     once the purchase settles. Teams whose limits staff customized keep
--     their team row untouched (apply_assignment_limits).
--  4. The new-team gate release pins the Free shape it expects; it now expects
--     this one.
-- Moves every open Free or legacy pay-as-you-go assignment still below the
-- curated template shape onto the published version of its plan. Safe to
-- call repeatedly; the Free allowance sweep does, so a team that kept its
-- assignment while a purchase was in flight moves once that settles.
CREATE OR REPLACE FUNCTION billing_raise_plan_shape(OUT moved integer, OUT skipped integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE
 free_id uuid; free_version integer;
 payg_id uuid; payg_version integer;
 a team_plan_assignment;
 at_time timestamptz;
 in_flight boolean;
BEGIN
 moved:=0; skipped:=0;
 SELECT id,version INTO free_id,free_version FROM billing_plan_version
  WHERE family='free' AND pricing_version='v2' AND state='published';
 SELECT id,version INTO payg_id,payg_version FROM billing_plan_version
  WHERE family='payg' AND pricing_version='legacy' AND kind='legacy_payg' AND state='published';
 FOR a IN SELECT p.* FROM team_plan_assignment p
  WHERE p.kind IN ('free','legacy_payg') AND p.effective_to IS NULL AND p.contract_id IS NULL
   -- An absent or null cap is no cap: it already admits the shape.
   AND (billing_json_cap(p.limits->'max_vcpu')<2
    OR billing_json_cap(p.limits->'max_memory_mib')<4096
    OR billing_json_cap(p.limits->'max_disk_mib')<8192)
  ORDER BY p.team_id
 LOOP
  -- One team's frozen invoice history must not hold back every other team;
  -- such a team keeps its old caps and is reported for a manual follow-up.
  BEGIN
   -- Same order as financial operations: invoice serialization, then team.
   PERFORM pg_advisory_xact_lock(hashtextextended('invoice:'||a.team_id::text,0));
   PERFORM 1 FROM team WHERE id=a.team_id FOR UPDATE;
   PERFORM 1 FROM team_plan_assignment WHERE id=a.id AND effective_to IS NULL;
   IF NOT FOUND THEN CONTINUE; END IF;
   -- A purchase in flight snapshots the current assignment id and refuses to
   -- activate against a different one. Such a team keeps its caps until the
   -- purchase settles; the paid plan it lands on carries larger ones.
   SELECT EXISTS(SELECT 1 FROM billing_commercial_contract WHERE team_id=a.team_id AND status<>'ended')
    OR EXISTS(SELECT 1 FROM billing_pro_upgrade WHERE team_id=a.team_id AND state NOT IN ('expired','ended'))
    OR EXISTS(SELECT 1 FROM billing_signup_choice WHERE team_id=a.team_id AND state='pending' AND offer_key<>'free')
    OR EXISTS(SELECT 1 FROM team_billing_account WHERE team_id=a.team_id AND (checkout_initializing_at IS NOT NULL OR checkout_may_exist))
    OR EXISTS(SELECT 1 FROM billing_prepaid_purchase WHERE team_id=a.team_id AND (payment_state IN ('pending','uncertain','processing') OR credit_state IN ('pending','uncertain')))
    OR EXISTS(SELECT 1 FROM team_plan_change WHERE team_id=a.team_id AND state IN ('scheduled','applied'))
   INTO in_flight;
   IF in_flight THEN
    skipped:=skipped+1;
    CONTINUE;
   END IF;
   at_time:=clock_timestamp();
   UPDATE team_plan_assignment SET effective_to=at_time WHERE id=a.id;
   INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason,idempotency_key)
   VALUES(a.team_id,a.plan_key,a.kind,
    CASE a.kind WHEN 'free' THEN COALESCE(free_id,a.plan_version_id) ELSE COALESCE(payg_id,a.plan_version_id) END,
    -- Without a published replacement the frozen version keys stay as they are.
    a.terms||CASE
     WHEN a.kind='free' AND free_id IS NOT NULL THEN jsonb_build_object('version_id',free_id,'version',free_version)
     WHEN a.kind<>'free' AND payg_id IS NOT NULL THEN jsonb_build_object('version_id',payg_id,'version',payg_version)
     ELSE '{}'::jsonb END,
    -- GREATEST ignores NULL, so a missing cap must stay missing explicitly.
    a.limits||jsonb_strip_nulls(jsonb_build_object(
     'max_vcpu',CASE WHEN billing_json_cap(a.limits->'max_vcpu')<2 THEN 2 END,
     'max_memory_mib',CASE WHEN billing_json_cap(a.limits->'max_memory_mib')<4096 THEN 4096 END,
     'max_disk_mib',CASE WHEN billing_json_cap(a.limits->'max_disk_mib')<8192 THEN 8192 END)),
    at_time,'migration','Plan shape raised to 2 vCPU / 4096 MiB / 8192 MiB','free-shape-2-4-8:'||a.id);
   moved:=moved+1;
  EXCEPTION WHEN OTHERS THEN
   RAISE WARNING 'free shape: team % kept its plan caps: %',a.team_id,SQLERRM;
  END;
 END LOOP;
END $$;
-- The allowance sweep retries the raise for teams skipped mid-purchase; this
-- keeps each retry to the rows still below the shape.
CREATE INDEX team_plan_assignment_below_shape ON team_plan_assignment(team_id)
 WHERE effective_to IS NULL AND contract_id IS NULL AND kind IN ('free','legacy_payg')
  AND (billing_json_cap(limits->'max_vcpu')<2 OR billing_json_cap(limits->'max_memory_mib')<4096
   OR billing_json_cap(limits->'max_disk_mib')<8192);
REVOKE ALL ON FUNCTION billing_raise_plan_shape() FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON FUNCTION billing_raise_plan_shape() FROM %I',r);
   IF r='service_role' THEN EXECUTE format('GRANT EXECUTE ON FUNCTION billing_raise_plan_shape() TO %I',r); END IF;
  END IF;
 END LOOP;
END $$;

DO $$
DECLARE
 old billing_plan_version;
 free_id uuid; free_version integer;
 payg_id uuid; payg_version integer;
 moved integer; skipped integer;
BEGIN
 -- Once the new-team cohort is live, every catalog write must leave a
 -- zero-template Free published unless paid-first signup has replaced it
 -- (billing_keep_cohort_free_catalog). Say so up front instead of failing at
 -- commit.
 IF (billing_new_team_ready() OR EXISTS(SELECT 1 FROM billing_new_team_enrollment))
  AND NOT (billing_paid_first_sellable() AND billing_trial_configured())
  AND NOT EXISTS(SELECT 1 FROM billing_plan_version v WHERE v.family='free' AND v.pricing_version='v2'
   AND v.state='published' AND v.kind='free' AND v.max_templates=0) THEN
  RAISE EXCEPTION 'publish the zero-template Free catalog before raising the Free shape' USING ERRCODE='55000';
 END IF;

 -- 1. v2 Free catalog.
 SELECT * INTO old FROM billing_plan_version
  WHERE family='free' AND pricing_version='v2' AND state='published' FOR UPDATE;
 IF FOUND AND (COALESCE(old.max_template_vcpu,0)<2 OR COALESCE(old.max_template_memory_mib,0)<4096
   OR COALESCE(old.max_template_disk_mib,0)<8192) THEN
  SELECT max(version)+1 INTO free_version FROM billing_plan_version WHERE family='free';
  free_id:=md5('billing-plan-version:v2:free:'||free_version)::uuid;
  INSERT INTO billing_plan_version SELECT (jsonb_populate_record(NULL::billing_plan_version,to_jsonb(old)||jsonb_build_object(
   'id',free_id,'version',free_version,'state','draft',
   'max_template_vcpu',GREATEST(old.max_template_vcpu,2),
   'max_template_memory_mib',GREATEST(old.max_template_memory_mib,4096),
   'max_template_disk_mib',GREATEST(old.max_template_disk_mib,8192),
   -- The allowance keeps its compute basis, so at the new shape it no
   -- longer lasts two hours; stop promising that on the plan card.
   'features',to_jsonb(COALESCE((SELECT array_agg(CASE WHEN f='2 hours of compute every day' THEN 'Daily compute allowance' ELSE f END ORDER BY o)
    FROM unnest(old.features) WITH ORDINALITY AS u(f,o)),'{}'::text[])),
   'created_by',NULL,'created_at',clock_timestamp(),'updated_at',clock_timestamp(),
   'published_by',NULL,'published_at',NULL,'retired_by',NULL,'retired_at',NULL))).*;
  PERFORM publish_billing_plan_version(free_id,NULL);
 END IF;
 SELECT id,version INTO free_id,free_version FROM billing_plan_version
  WHERE family='free' AND pricing_version='v2' AND state='published';

 -- 2. Legacy pay-as-you-go fallback.
 SELECT * INTO old FROM billing_plan_version
  WHERE family='payg' AND pricing_version='legacy' AND kind='legacy_payg' AND state='published' FOR UPDATE;
 IF FOUND AND (COALESCE(old.max_template_vcpu,0)<2 OR COALESCE(old.max_template_memory_mib,0)<4096
   OR COALESCE(old.max_template_disk_mib,0)<8192) THEN
  SELECT max(version)+1 INTO payg_version FROM billing_plan_version WHERE family='payg';
  payg_id:=md5('billing-plan-version:legacy:payg:'||payg_version)::uuid;
  INSERT INTO billing_plan_version SELECT (jsonb_populate_record(NULL::billing_plan_version,to_jsonb(old)||jsonb_build_object(
   'id',payg_id,'version',payg_version,'state','draft',
   'max_template_vcpu',GREATEST(old.max_template_vcpu,2),
   'max_template_memory_mib',GREATEST(old.max_template_memory_mib,4096),
   'max_template_disk_mib',GREATEST(old.max_template_disk_mib,8192),
   'created_by',NULL,'created_at',clock_timestamp(),'updated_at',clock_timestamp(),
   'published_by',NULL,'published_at',NULL,'retired_by',NULL,'retired_at',NULL))).*;
  PERFORM publish_billing_plan_version(payg_id,NULL);
 END IF;
 SELECT id,version INTO payg_id,payg_version FROM billing_plan_version
  WHERE family='payg' AND pricing_version='legacy' AND kind='legacy_payg' AND state='published';

 -- 3. Open assignments still below the shape.
 SELECT * INTO moved,skipped FROM billing_raise_plan_shape();
 RAISE NOTICE 'free shape: % open assignments moved to the new shape, % kept for a purchase in flight',moved,skipped;
END $$;

-- 4. The new-team gate release keeps every launch condition: sellable
-- Developer and Pro with a configured trial, or a usable Free whose caps fit
-- the curated template shape, plus the cohort, pending-payment and Free
-- enforcement schema.
CREATE OR REPLACE FUNCTION billing_release_new_team_gate(p_region text,p_system uuid,p_epoch uuid,p_actor text,p_reason text,p_evidence jsonb)
RETURNS uuid LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate; raw text;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN RAISE EXCEPTION 'release requires READ COMMITTED isolation'; END IF;
 SELECT pricing_version INTO STRICT raw FROM billing_pricing_switch WHERE singleton FOR SHARE;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton FOR UPDATE;
 IF raw<>'legacy' OR g.state<>'closed' OR p_epoch IS DISTINCT FROM g.epoch OR p_region IS DISTINCT FROM g.region OR p_system IS DISTINCT FROM g.system_team_id
  OR COALESCE(btrim(p_actor),'')='' OR COALESCE(btrim(p_reason),'')='' THEN
  RAISE EXCEPTION 'matching closed new-team cell and epoch required' USING ERRCODE='55000';
 END IF;
 g.evidence:=p_evidence;
 IF NOT billing_new_team_evidence_valid(g) THEN RAISE EXCEPTION 'verified cohort-contract writer and schema evidence required' USING ERRCODE='55000'; END IF;
 -- A trial must admit the curated template shape, like Free, or a trial
 -- team's first sandbox is refused.
 IF NOT (billing_paid_first_sellable() AND billing_trial_configured() AND EXISTS(SELECT 1 FROM billing_trial_policy
   WHERE singleton AND max_vcpu>=2 AND max_memory_mib>=4096 AND max_disk_mib>=8192)) AND NOT EXISTS(SELECT 1 FROM billing_plan_version v
   WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published' AND v.kind='free' AND v.max_templates=0
    AND v.allowance_period='day_window' AND v.allowance_on_exhausted='pause' AND v.allowance_compute_seconds>0
    AND v.allowance_compute_vcpu>0 AND v.allowance_compute_memory_gib>0 AND NOT v.display_only AND NOT v.self_serve
    AND v.max_template_vcpu>=2 AND v.max_template_memory_mib>=4096 AND v.max_template_disk_mib>=8192) THEN
  RAISE EXCEPTION 'a usable Free catalog, or sellable Developer and Pro with a configured trial, required' USING ERRCODE='55000';
 END IF;
 IF to_regprocedure('billing_lock_team_pricing_state(uuid)') IS NULL OR to_regprocedure('billing_team_awaiting_plan(uuid)') IS NULL
  OR to_regprocedure('billing_free_allowance_refresh(uuid)') IS NULL OR to_regprocedure('billing_free_plan(uuid)') IS NULL
  OR to_regprocedure('request_team_pause_all(uuid,text)') IS NULL
  OR NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='team_plan_assignment'::regclass AND attname='free_allowance_since' AND NOT attisdropped)
  OR NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='team'::regclass AND tgname='team_new_free_plan' AND tgenabled IN ('O','A') AND tgfoid='assign_new_team_free_plan()'::regprocedure)
  OR NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='sandbox'::regclass AND tgname='sandbox_free_shape' AND tgenabled IN ('O','A')) THEN
  RAISE EXCEPTION 'cohort, pending-payment and Free enforcement schema required' USING ERRCODE='55000';
 END IF;
 UPDATE billing_new_team_gate SET state='released',evidence=p_evidence,released_at=clock_timestamp() WHERE singleton;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details) VALUES(g.epoch,g.region,'release',p_actor,p_reason,p_evidence);
 RETURN g.epoch;
END $$;
