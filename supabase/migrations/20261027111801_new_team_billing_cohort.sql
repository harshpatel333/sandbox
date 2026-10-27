-- New signups are an independent regional cohort. Historical teams and the
-- all-team adoption barrier deliberately retain their existing authority.
CREATE TABLE billing_new_team_gate (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 region text CHECK(region IN ('use','usw')),
 system_team_id uuid CHECK(system_team_id<>'00000000-0000-0000-0000-000000000000'),
 epoch uuid NOT NULL DEFAULT gen_random_uuid(),
 state text NOT NULL DEFAULT 'closed' CHECK(state IN ('closed','released')),
 evidence jsonb,
 released_at timestamptz,
 CHECK((region IS NULL)=(system_team_id IS NULL)),
 CHECK((state='released')=(released_at IS NOT NULL)),
 CHECK(state<>'released' OR (region IS NOT NULL AND evidence IS NOT NULL))
);
INSERT INTO billing_new_team_gate(singleton) VALUES(true);
CREATE TABLE billing_new_team_enrollment (
 team_id uuid PRIMARY KEY,
 region text NOT NULL CHECK(region IN ('use','usw')),
 enrollment_epoch uuid NOT NULL,
 initial_assignment_id uuid NOT NULL UNIQUE REFERENCES team_plan_assignment(id) DEFERRABLE INITIALLY DEFERRED,
 initial_free_version_id uuid NOT NULL REFERENCES billing_plan_version(id),
 creator_user_id uuid NOT NULL,
 source text NOT NULL CHECK(source IN ('trusted_signup','trusted_creation_attempt')),
 enrolled_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE billing_new_team_gate_event (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), epoch uuid NOT NULL,
 region text, action text NOT NULL, actor text NOT NULL, reason text NOT NULL,
 details jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER billing_new_team_enrollment_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON billing_new_team_enrollment
 FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_append_only();
CREATE TRIGGER billing_new_team_event_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON billing_new_team_gate_event
 FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_append_only();
CREATE TRIGGER billing_new_team_gate_retained BEFORE DELETE OR TRUNCATE ON billing_new_team_gate
 FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_append_only();

CREATE FUNCTION billing_new_team_evidence_valid(g billing_new_team_gate) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT COALESCE(g.region IN ('use','usw') AND g.system_team_id IS NOT NULL
  AND g.evidence->>'region'=g.region AND g.evidence->>'system_team_id'=g.system_team_id::text
  AND g.evidence->>'epoch'=g.epoch::text AND g.evidence->>'cohort_contract'='1'
  AND g.evidence->>'assembled_sha' ~ '^[0-9a-f]{40}$'
  AND g.evidence->>'schema_sha'=g.evidence->>'assembled_sha'
  AND jsonb_typeof(g.evidence->'writers')='array' AND jsonb_array_length(g.evidence->'writers')>=3
  AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(g.evidence->'writers')='array' THEN g.evidence->'writers' ELSE '[]' END) w
    WHERE NOT COALESCE(w->>'role' IN ('api','workers','console') AND w->>'cohort_contract'='1'
     AND w->'verified'='true'::jsonb AND w->>'source_sha' ~ '^[0-9a-f]{40}$'
     AND (w->>'role'='console' OR w->>'source_sha'=g.evidence->>'assembled_sha')
     AND length(btrim(w->>'reference'))>0 AND length(btrim(w->>'digest'))>0,false))
  AND (SELECT count(DISTINCT w->>'role')=3 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(g.evidence->'writers')='array' THEN g.evidence->'writers' ELSE '[]' END) w),false);
$$;
CREATE FUNCTION billing_new_team_ready() RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT COALESCE((SELECT g.state='released' AND g.released_at IS NOT NULL AND billing_new_team_evidence_valid(g)
  FROM billing_new_team_gate g WHERE singleton),false);
$$;

CREATE FUNCTION billing_prepare_new_team_gate(p_region text,p_system uuid,p_actor text,p_reason text)
RETURNS uuid LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate; raw text;
BEGIN
 SELECT pricing_version INTO STRICT raw FROM billing_pricing_switch WHERE singleton FOR SHARE;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton FOR UPDATE;
 IF raw<>'legacy' OR g.state<>'closed' OR p_region IS NULL OR p_region NOT IN ('use','usw')
  OR p_system IS NULL OR NOT EXISTS(SELECT 1 FROM team WHERE id=p_system)
  OR COALESCE(btrim(p_actor),'')='' OR COALESCE(btrim(p_reason),'')='' THEN
  RAISE EXCEPTION 'closed raw legacy gate, region, existing system team, actor and reason required' USING ERRCODE='55000';
 END IF;
 IF g.region IS NOT NULL AND (g.region<>p_region OR g.system_team_id<>p_system) THEN
  RAISE EXCEPTION 'new team gate belongs to another cell' USING ERRCODE='55000';
 END IF;
 UPDATE billing_new_team_gate SET region=p_region,system_team_id=p_system,epoch=gen_random_uuid(),evidence=NULL,released_at=NULL WHERE singleton RETURNING * INTO g;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details) VALUES(g.epoch,g.region,'prepare',p_actor,p_reason,'{}');
 RETURN g.epoch;
END $$;
CREATE FUNCTION billing_release_new_team_gate(p_region text,p_system uuid,p_epoch uuid,p_actor text,p_reason text,p_evidence jsonb)
RETURNS uuid LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate; f billing_plan_version; raw text;
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
 SELECT * INTO f FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published' FOR SHARE;
 IF NOT FOUND OR f.kind<>'free' OR f.max_templates IS DISTINCT FROM 0 OR f.allowance_period IS DISTINCT FROM 'day_window'
  OR f.allowance_on_exhausted IS DISTINCT FROM 'pause' OR f.allowance_compute_seconds IS DISTINCT FROM 7200
  OR f.allowance_compute_vcpu IS DISTINCT FROM 1 OR f.allowance_compute_memory_gib IS DISTINCT FROM 1
  OR f.max_sandboxes IS DISTINCT FROM 1 OR f.max_template_vcpu IS DISTINCT FROM 1 OR f.max_template_memory_mib IS DISTINCT FROM 1024
  OR f.max_template_disk_mib IS NULL OR f.display_only OR f.self_serve THEN
  RAISE EXCEPTION 'usable zero-template two-hour Free catalog required' USING ERRCODE='55000';
 END IF;
 IF to_regprocedure('billing_free_allowance_refresh(uuid)') IS NULL OR to_regprocedure('billing_free_plan(uuid)') IS NULL
  OR to_regprocedure('request_team_pause_all(uuid,text)') IS NULL OR to_regprocedure('billing_lock_team_pricing_state(uuid)') IS NULL
  OR NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='team_plan_assignment'::regclass AND attname='free_allowance_since' AND NOT attisdropped)
  OR NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='team'::regclass AND tgname='team_new_free_plan' AND tgenabled IN ('O','A') AND tgfoid='assign_new_team_free_plan()'::regprocedure)
  OR NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='sandbox'::regclass AND tgname='sandbox_free_shape' AND tgenabled IN ('O','A')) THEN
  RAISE EXCEPTION 'cohort, Free enforcement and allowance worker schema required' USING ERRCODE='55000';
 END IF;
 UPDATE billing_new_team_gate SET state='released',evidence=p_evidence,released_at=clock_timestamp() WHERE singleton;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details) VALUES(g.epoch,g.region,'release',p_actor,p_reason,p_evidence);
 RETURN g.epoch;
END $$;
CREATE FUNCTION billing_close_new_team_gate(p_epoch uuid,p_actor text,p_reason text) RETURNS uuid
LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate;
BEGIN
 PERFORM 1 FROM billing_pricing_switch WHERE singleton FOR SHARE;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton FOR UPDATE;
 IF p_epoch IS DISTINCT FROM g.epoch OR COALESCE(btrim(p_actor),'')='' OR COALESCE(btrim(p_reason),'')='' THEN
  RAISE EXCEPTION 'current epoch, actor and reason required' USING ERRCODE='55000';
 END IF;
 UPDATE billing_new_team_gate SET state='closed',epoch=gen_random_uuid(),evidence=NULL,released_at=NULL WHERE singleton RETURNING * INTO g;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details) VALUES(g.epoch,g.region,'close',p_actor,p_reason,'{}');
 RETURN g.epoch;
END $$;

-- A read and locked read share exactly the same model resolution. Enrollment
-- survives closing/rotating the current gate and changing the current plan.
CREATE FUNCTION billing_team_pricing_state(p_team uuid)
RETURNS TABLE(pricing_version text,admission_source text,entry_open boolean,epoch uuid,enrollment_epoch uuid)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE raw text; g billing_new_team_gate; e billing_new_team_enrollment; home text; a record;
BEGIN
 SELECT t.home_region INTO STRICT home FROM team t WHERE id=p_team;
 SELECT s.pricing_version INTO STRICT raw FROM billing_pricing_switch s WHERE singleton;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton;
 SELECT * INTO e FROM billing_new_team_enrollment n WHERE n.team_id=p_team;
 IF FOUND AND (e.region<>home OR e.region IS DISTINCT FROM g.region
  OR NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.id=e.initial_assignment_id AND p.team_id=p_team AND p.kind='free'
     AND p.source='signup' AND p.plan_version_id=e.initial_free_version_id AND p.effective_from=e.enrolled_at)
  OR NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.team_id=p_team AND p.effective_to IS NULL)) THEN
  RAISE EXCEPTION 'enrolled team billing authority requires verification' USING ERRCODE='55000';
 END IF;
 IF to_regclass('public.billing_pricing_adoption') IS NULL THEN
  SELECT 'legacy'::text AS pricing_version,false AS entry_open,NULL::uuid AS epoch INTO a;
 ELSE
  SELECT * INTO a FROM billing_pricing_public_state();
 END IF;
 IF raw='v2' THEN
  RETURN QUERY SELECT CASE WHEN a.entry_open OR e.team_id IS NOT NULL THEN 'v2' ELSE a.pricing_version END,'all_teams'::text,a.entry_open,a.epoch,e.enrollment_epoch;
 ELSIF e.team_id IS NOT NULL THEN
  RETURN QUERY SELECT 'v2'::text,'new_team'::text,billing_new_team_ready(),g.epoch,e.enrollment_epoch;
 ELSE
  RETURN QUERY SELECT 'legacy'::text,'legacy'::text,true,NULL::uuid,NULL::uuid;
 END IF;
END $$;
CREATE FUNCTION billing_lock_team_pricing_state(p_team uuid)
RETURNS TABLE(pricing_version text,admission_source text,entry_open boolean,epoch uuid,enrollment_epoch uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 PERFORM 1 FROM billing_pricing_switch WHERE singleton FOR SHARE;
 IF to_regclass('public.billing_pricing_adoption') IS NOT NULL THEN
  PERFORM 1 FROM billing_pricing_adoption WHERE singleton FOR SHARE;
 END IF;
 PERFORM 1 FROM billing_new_team_gate WHERE singleton FOR SHARE;
 RETURN QUERY SELECT * FROM billing_team_pricing_state(p_team);
END $$;

CREATE FUNCTION billing_assert_team_pricing_entry(p_team uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE s record; result uuid;
BEGIN
 SELECT * INTO s FROM billing_lock_team_pricing_state(p_team);
 IF s.pricing_version<>'v2' OR NOT s.entry_open THEN
  RAISE EXCEPTION 'team pricing entry is closed' USING ERRCODE='55000';
 END IF;
 IF s.admission_source='all_teams' THEN
  SELECT billing_assert_pricing_entry_open(s.epoch,'v2') INTO result;
 ELSE
  PERFORM billing_check_cohort_admission(p_team,s.admission_source,s.epoch);
  result:=s.epoch;
 END IF;
 RETURN result;
END $$;

-- Only trusted existing creation RPCs can reserve enrollment, before the
-- actual INSERT. Deferred validation makes missing or mismatched assignment
-- authority uncommittable, without trusting created_at or a caller GUC.
CREATE FUNCTION billing_insert_signup_team(p_id uuid,p_name text,p_actor uuid,p_region text,p_source text)
RETURNS team LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate; f billing_plan_version; result team;
BEGIN
 IF p_id IS NULL OR p_actor IS NULL OR p_region IS NULL OR p_region NOT IN ('use','usw') OR p_source IS NULL OR p_source NOT IN ('trusted_signup','trusted_creation_attempt') THEN
  RAISE EXCEPTION 'trusted signup identity required' USING ERRCODE='22023';
 END IF;
 PERFORM 1 FROM billing_pricing_switch WHERE singleton FOR SHARE;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton FOR SHARE;
 IF (SELECT pricing_version FROM billing_pricing_switch WHERE singleton)='legacy' AND g.state='released' AND NOT billing_new_team_ready() THEN
  RAISE EXCEPTION 'new-team gate requires verified writer authority' USING ERRCODE='55000';
 END IF;
 IF (SELECT pricing_version FROM billing_pricing_switch WHERE singleton)='legacy' AND billing_new_team_ready() THEN
  IF p_region<>g.region OR p_id=g.system_team_id OR qm_team_managed_billing(p_id) THEN
   RAISE EXCEPTION 'signup region or identity does not match new-team cell' USING ERRCODE='55000';
  END IF;
  SELECT * INTO STRICT f FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published' FOR SHARE;
  INSERT INTO billing_new_team_enrollment(team_id,region,enrollment_epoch,initial_assignment_id,initial_free_version_id,creator_user_id,source)
   VALUES(p_id,p_region,g.epoch,gen_random_uuid(),f.id,p_actor,p_source);
 END IF;
 INSERT INTO team(id,name,home_region) VALUES(p_id,p_name,p_region) RETURNING * INTO result;
 RETURN result;
END $$;
CREATE FUNCTION billing_validate_new_team_enrollment() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM team t JOIN team_plan_assignment p ON p.team_id=t.id
  WHERE t.id=NEW.team_id AND t.home_region=NEW.region AND p.id=NEW.initial_assignment_id
   AND p.plan_version_id=NEW.initial_free_version_id AND p.kind='free' AND p.source='signup'
   AND p.terms->>'pricing_version'='v2' AND p.effective_from=NEW.enrolled_at) THEN
  RAISE EXCEPTION 'new-team enrollment must commit with its actual Free assignment' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER billing_new_team_enrollment_complete AFTER INSERT ON billing_new_team_enrollment
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing_validate_new_team_enrollment();

CREATE OR REPLACE FUNCTION assign_new_team_free_plan() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE v billing_plan_version; pricing text; g billing_new_team_gate; e billing_new_team_enrollment;
BEGIN
 SELECT pricing_version INTO pricing FROM billing_pricing_switch FOR SHARE;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton FOR SHARE;
 SELECT * INTO e FROM billing_new_team_enrollment WHERE team_id=NEW.id;
 IF current_setting('superserve.migrating_team_id',true)=NEW.id::text THEN
  IF pricing='v2' OR billing_new_team_ready() THEN RAISE EXCEPTION 'regional team import is unavailable while pricing v2 is active' USING ERRCODE='55000'; END IF;
  RETURN NULL;
 END IF;
 IF pricing IS DISTINCT FROM 'v2' THEN
  IF g.state='released' AND NOT billing_new_team_ready() THEN RAISE EXCEPTION 'new-team gate requires verified writer authority' USING ERRCODE='55000'; END IF;
  IF NOT billing_new_team_ready() THEN
   IF e.team_id IS NOT NULL THEN RAISE EXCEPTION 'new-team gate closed during signup' USING ERRCODE='55000'; END IF;
   RETURN NULL;
  END IF;
  IF NEW.id=g.system_team_id OR (NEW.home_region=g.region AND qm_team_managed_billing(NEW.id)) THEN RETURN NULL; END IF;
  IF e.team_id IS NULL OR e.region<>NEW.home_region OR e.region<>g.region OR e.enrollment_epoch<>g.epoch THEN
   RAISE EXCEPTION 'trusted actual new-team creation required' USING ERRCODE='55000';
  END IF;
 END IF;
 SELECT * INTO v FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published' FOR SHARE;
 IF NOT FOUND THEN RAISE EXCEPTION 'published v2 Free plan is required for team creation' USING ERRCODE='55000'; END IF;
 INSERT INTO team_plan_assignment(id,team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason)
 VALUES(COALESCE(e.initial_assignment_id,gen_random_uuid()),NEW.id,v.family,v.kind,v.id,
  jsonb_build_object('offer_key',v.family,'display_name',v.display_name,'fee_cents',0,'interval_months',0,
   'monthly_credit_cents',COALESCE(billing_plan_version_monthly_credit(v),0),'version_id',v.id,'version',v.version,
   'pricing_version',v.pricing_version,'kind',v.kind,'family',v.family,'grants',v.credit_schedule,
   'matching',CASE WHEN v.matching_bps IS NULL THEN NULL ELSE jsonb_build_object('matching_bps',v.matching_bps,'cap_cents',v.cap_cents) END,
   'allowance',CASE WHEN v.allowance_period IS NULL THEN NULL ELSE jsonb_strip_nulls(jsonb_build_object('amount_cents',COALESCE(v.allowance_cents,0),
    'period',v.allowance_period,'on_exhausted',v.allowance_on_exhausted,
    'compute',CASE WHEN v.allowance_compute_seconds IS NULL THEN NULL ELSE jsonb_build_object('seconds',v.allowance_compute_seconds,
     'vcpu',v.allowance_compute_vcpu,'memory_gib',v.allowance_compute_memory_gib) END)) END),
  jsonb_build_object('max_concurrent',v.max_sandboxes,'max_vcpu',v.max_template_vcpu,'max_memory_mib',v.max_template_memory_mib,
   'max_disk_mib',v.max_template_disk_mib,'max_templates',v.max_templates),
  COALESCE(e.enrolled_at,clock_timestamp()),'signup','new team');
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION create_team_with_signup_trial(p_name text, p_user_id uuid, p_home_region text)
RETURNS team LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE created_team team;
BEGIN
    SELECT * INTO created_team FROM billing_insert_signup_team(gen_random_uuid(),p_name,p_user_id,p_home_region,'trusted_signup');
    PERFORM claim_team_signup_trial(created_team.id, p_user_id);
    DELETE FROM team_signup_trial_provenance WHERE team_id = created_team.id;
    RETURN created_team;
END $$;


CREATE OR REPLACE FUNCTION create_team_with_signup_trial(
    p_name text,
    p_user_id uuid,
    p_home_region text,
    p_policy_mode text
)
RETURNS team
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    created_team team;
BEGIN
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'signup trial requires a creator' USING ERRCODE = '22023';
    END IF;
    IF p_policy_mode IS NULL OR p_policy_mode NOT IN ('first_team', 'additional_team') THEN
        RAISE EXCEPTION 'unsupported team creation policy' USING ERRCODE = '22023';
    END IF;
    SELECT * INTO created_team FROM billing_insert_signup_team(gen_random_uuid(),p_name,p_user_id,p_home_region,'trusted_signup');
    PERFORM claim_team_signup_trial_with_device(created_team.id, p_user_id);
    DELETE FROM team_signup_trial_provenance WHERE team_id = created_team.id;
    RETURN created_team;
END;
$$;


CREATE OR REPLACE FUNCTION create_team_with_promotion_attempt(p_attempt_id uuid, p_team_id uuid,
    p_user_id uuid, p_name text, p_home_region text, p_authority_unavailable boolean)
RETURNS TABLE(team_id uuid, outcome text, reason text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE v_attempt team_promotion_creation_attempt; v_outcome text; v_reason text;
BEGIN
    IF p_attempt_id IS NULL OR p_team_id IS NULL OR p_user_id IS NULL
       OR NULLIF(BTRIM(p_name),'') IS NULL OR p_home_region NOT IN ('use','usw')
       OR p_home_region IS NULL OR p_authority_unavailable IS NULL THEN
        RAISE EXCEPTION 'invalid team creation attempt' USING ERRCODE='22023';
    END IF;
    INSERT INTO team_promotion_creation_attempt(attempt_id,team_id,user_id,name,home_region,authority_unavailable)
        VALUES(p_attempt_id,p_team_id,p_user_id,p_name,p_home_region,p_authority_unavailable)
        ON CONFLICT (attempt_id) DO NOTHING;
    SELECT * INTO STRICT v_attempt FROM team_promotion_creation_attempt a
        WHERE a.attempt_id=p_attempt_id FOR UPDATE;
    IF v_attempt.team_id<>p_team_id OR v_attempt.user_id<>p_user_id OR v_attempt.name<>p_name
       OR v_attempt.home_region<>p_home_region OR v_attempt.authority_unavailable<>p_authority_unavailable THEN
        RAISE EXCEPTION 'team creation attempt cannot change' USING ERRCODE='22023';
    END IF;
    IF v_attempt.outcome IS NOT NULL THEN
        RETURN QUERY SELECT v_attempt.team_id,v_attempt.outcome,v_attempt.reason;
        RETURN;
    END IF;
    -- An existing team is never adopted by a new attempt.
    PERFORM billing_insert_signup_team(p_team_id,p_name,p_user_id,p_home_region,'trusted_creation_attempt');
    SELECT c.outcome,c.reason INTO STRICT v_outcome,v_reason
        FROM claim_team_signup_trial(p_team_id,p_user_id) c;
    DELETE FROM team_signup_trial_provenance WHERE team_signup_trial_provenance.team_id=p_team_id;
    UPDATE team_promotion_creation_attempt a SET outcome=v_outcome,reason=v_reason
        WHERE a.attempt_id=p_attempt_id;
    RETURN QUERY SELECT p_team_id,v_outcome,v_reason;
END $$;

CREATE OR REPLACE FUNCTION claim_team_signup_trial_with_device(p_team_id uuid, p_user_id uuid)
RETURNS TABLE(outcome text, reason text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE v_reason text; v_fingerprint text; v_outcome text; v_claim_reason text; v_actor uuid;
    v_provenance team_signup_trial_provenance;
BEGIN
    PERFORM 1 FROM team WHERE id = p_team_id FOR NO KEY UPDATE NOWAIT;
    IF NOT FOUND THEN RAISE EXCEPTION 'signup team does not exist' USING ERRCODE = '22023'; END IF;
    SELECT o.user_id, o.outcome, o.reason INTO v_actor, v_outcome, v_claim_reason
        FROM team_signup_promotion_outcome o WHERE o.team_id = p_team_id;
    IF FOUND THEN
        IF v_actor <> p_user_id THEN
            RAISE EXCEPTION 'signup trial creator cannot change' USING ERRCODE = '22023';
        END IF;
        RETURN QUERY SELECT v_outcome, v_claim_reason;
        RETURN;
    END IF;
    SELECT * INTO v_provenance FROM team_signup_trial_provenance
        WHERE team_id = p_team_id FOR UPDATE;
    IF NOT FOUND OR v_provenance.completed_at IS NOT NULL THEN
        RETURN QUERY SELECT 'promotion_ineligible'::text, 'not_initial_creation'::text;
        RETURN;
    END IF;
    IF v_provenance.creator_bound_at IS NOT NULL
       AND v_provenance.creator_user_id IS DISTINCT FROM p_user_id THEN
        RAISE EXCEPTION 'signup trial creator cannot change' USING ERRCODE = '22023';
    END IF;
    IF p_user_id IS NULL THEN RAISE EXCEPTION 'signup trial requires a creator' USING ERRCODE = '22023'; END IF;
    IF billing_pricing_version() = 'v2' OR EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=p_team_id) THEN
        v_reason := 'pricing_v2';
    ELSIF EXISTS (SELECT 1 FROM team_promotion_creation_attempt a
        WHERE a.team_id = p_team_id AND a.user_id = p_user_id AND a.authority_unavailable) THEN
        v_reason := 'authority_unavailable';
    ELSE
        SET LOCAL lock_timeout = '5s';
        PERFORM pg_advisory_xact_lock(hashtext('stripe-promo-user:' || p_user_id::text)::bigint);
        PERFORM pg_advisory_xact_lock(hashtext('promotion-device-user:' || p_user_id::text)::bigint);
        BEGIN
            v_reason := promotion_device_decision(p_user_id, 'signup');
        EXCEPTION WHEN SQLSTATE '55000' OR no_data_found THEN
            v_reason := 'authority_unavailable';
        END;
    END IF;
    IF v_reason <> 'eligible' THEN
        IF v_provenance.legacy_grant_id IS NULL THEN
            INSERT INTO team_signup_trial_denial(team_id) VALUES(p_team_id) ON CONFLICT DO NOTHING;
        END IF;
        INSERT INTO team_signup_promotion_outcome(team_id, user_id, outcome, reason)
            VALUES(p_team_id, p_user_id, 'promotion_ineligible', v_reason);
        UPDATE team_signup_trial_provenance SET creator_user_id = p_user_id,
            creator_bound_at = COALESCE(creator_bound_at, now()), completed_at = now()
            WHERE team_id = p_team_id;
        RETURN QUERY SELECT 'promotion_ineligible'::text, v_reason;
        RETURN;
    END IF;
    BEGIN
        SELECT c.outcome, c.reason INTO v_outcome, v_claim_reason
            FROM claim_team_signup_trial_without_device(p_team_id, p_user_id) c;
    EXCEPTION WHEN SQLSTATE '55000' THEN
        -- A class condition also catches retryable lock errors such as 55P03.
        IF SQLSTATE <> '55000' THEN RAISE; END IF;
        v_outcome := 'promotion_ineligible';
        v_claim_reason := 'authority_unavailable';
        IF v_provenance.legacy_grant_id IS NULL THEN
            INSERT INTO team_signup_trial_denial(team_id) VALUES(p_team_id) ON CONFLICT DO NOTHING;
        END IF;
        INSERT INTO team_signup_promotion_outcome(team_id, user_id, outcome, reason)
            VALUES(p_team_id, p_user_id, v_outcome, v_claim_reason);
        UPDATE team_signup_trial_provenance SET creator_user_id = p_user_id,
            creator_bound_at = COALESCE(creator_bound_at, now()), completed_at = now()
            WHERE team_id = p_team_id;
        RETURN QUERY SELECT v_outcome, v_claim_reason;
        RETURN;
    END;
    IF v_outcome = 'granted' THEN
        SELECT e.fingerprint INTO v_fingerprint FROM promotion_signup_device_evidence e WHERE e.user_id = p_user_id;
        INSERT INTO promotion_device_grant(promotion, user_id, team_id, fingerprint)
            VALUES('signup', p_user_id, p_team_id, v_fingerprint) ON CONFLICT DO NOTHING;
    END IF;
    RETURN QUERY SELECT v_outcome, v_claim_reason;
END;
$$;


CREATE OR REPLACE FUNCTION reserve_stripe_promotion_with_device(p_team_id uuid, p_user_id uuid,
    p_event_id text, p_subscription_id text, p_checkout_generation timestamptz,
    p_has_checkout_generation boolean)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
    SET LOCAL lock_timeout = '5s';
    PERFORM pg_advisory_xact_lock(hashtext('stripe-promo-user:' || COALESCE(p_user_id::text, ''))::bigint);
    IF (billing_pricing_version() = 'v2' OR EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=p_team_id))
       AND NOT EXISTS (SELECT 1 FROM team_billing_account
           WHERE team_id = p_team_id AND stripe_activation_credit_reserved_at IS NOT NULL) THEN
        RETURN 'pricing_v2';
    END IF;
    RETURN reserve_stripe_promotion_before_pricing_v2(p_team_id, p_user_id, p_event_id,
        p_subscription_id, p_checkout_generation, p_has_checkout_generation);
END $$;



-- Stamps are additive; historical NULL rows keep their original settlement
-- semantics. New enrolled-team acceptance requires the current locked gate.
ALTER TABLE billing_signup_choice ADD COLUMN admission_source text, ADD COLUMN admission_epoch uuid;
ALTER TABLE billing_pro_upgrade ADD COLUMN admission_source text, ADD COLUMN admission_epoch uuid;
ALTER TABLE billing_prepaid_purchase ADD COLUMN admission_source text, ADD COLUMN admission_epoch uuid;
ALTER TABLE billing_prepaid_auto_recharge ADD COLUMN admission_source text, ADD COLUMN admission_epoch uuid;
ALTER TABLE team_billing_account ADD COLUMN checkout_admission_source text, ADD COLUMN checkout_admission_epoch uuid,
 ADD COLUMN checkout_admission_attempt uuid;

CREATE FUNCTION billing_check_cohort_admission(p_team uuid,p_source text,p_epoch uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE s record;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=p_team) THEN RETURN; END IF;
 -- Older writers can already hold account/team locks. Never wait in reverse
 -- gate order; retry before any provider dispatch instead.
 PERFORM 1 FROM billing_pricing_switch WHERE singleton FOR SHARE NOWAIT;
 PERFORM 1 FROM billing_pricing_adoption WHERE singleton FOR SHARE NOWAIT;
 PERFORM 1 FROM billing_new_team_gate WHERE singleton FOR SHARE NOWAIT;
 SELECT * INTO s FROM billing_team_pricing_state(p_team);
 IF s.pricing_version<>'v2' OR NOT s.entry_open OR p_source IS DISTINCT FROM s.admission_source OR p_epoch IS DISTINCT FROM s.epoch THEN
  RAISE EXCEPTION 'team pricing admission closed or changed' USING ERRCODE='55000';
 END IF;
END $$;
CREATE FUNCTION billing_guard_cohort_commercial_entry() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE enrolled boolean; fresh boolean:=false;
BEGIN
 SELECT EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=NEW.team_id) INTO enrolled;
 IF TG_TABLE_NAME='billing_signup_choice' THEN
  IF TG_OP='INSERT' THEN fresh:=NEW.state='pending' AND NEW.offer_key NOT IN ('free','starter');
  ELSE fresh:=NEW.state='pending' AND NEW.offer_key NOT IN ('free','starter') AND
   (NEW.generation IS DISTINCT FROM OLD.generation OR OLD.state<>'pending'); END IF;
  IF enrolled AND fresh THEN
   IF NEW.request_payload->'PlanTerms'->'PricingV2' IS DISTINCT FROM 'true'::jsonb THEN
    RAISE EXCEPTION 'enrolled Pro requires frozen v2 terms' USING ERRCODE='55000';
   END IF;
   PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
  ELSIF TG_OP='UPDATE' AND NOT fresh AND (NEW.admission_source,NEW.admission_epoch) IS DISTINCT FROM (OLD.admission_source,OLD.admission_epoch) THEN
   -- A paid upgrade may publish its already accepted choice after gate close.
   IF NOT EXISTS(SELECT 1 FROM billing_pro_upgrade u WHERE u.team_id=NEW.team_id AND u.actor_id=NEW.actor_id
    AND u.request_payload=NEW.request_payload AND u.admission_source IS NOT DISTINCT FROM NEW.admission_source
    AND u.admission_epoch IS NOT DISTINCT FROM NEW.admission_epoch) THEN
    RAISE EXCEPTION 'accepted pricing admission is immutable' USING ERRCODE='23514';
   END IF;
  END IF;
 ELSIF TG_TABLE_NAME='billing_prepaid_auto_recharge' THEN
  IF TG_OP='INSERT' THEN fresh:=NEW.enabled;
  ELSE fresh:=NEW.enabled AND (NOT OLD.enabled OR (NEW.threshold_cents,NEW.amount_cents,NEW.updated_by) IS DISTINCT FROM (OLD.threshold_cents,OLD.amount_cents,OLD.updated_by)); END IF;
  IF fresh THEN PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
  ELSIF TG_OP='UPDATE' AND (NEW.admission_source,NEW.admission_epoch) IS DISTINCT FROM (OLD.admission_source,OLD.admission_epoch) THEN
   RAISE EXCEPTION 'saved recharge admission cannot change without new consent' USING ERRCODE='23514';
  END IF;
 ELSE
  IF TG_OP='UPDATE' THEN
   IF (NEW.admission_source,NEW.admission_epoch) IS DISTINCT FROM (OLD.admission_source,OLD.admission_epoch) THEN
    RAISE EXCEPTION 'accepted pricing admission is immutable' USING ERRCODE='23514';
   END IF;
  ELSIF enrolled THEN
   IF TG_TABLE_NAME='billing_prepaid_purchase' THEN
    IF NEW.source='auto_recharge' THEN
    IF NOT EXISTS(SELECT 1 FROM billing_prepaid_auto_recharge a WHERE a.team_id=NEW.team_id AND a.enabled
     AND a.admission_source IS NOT DISTINCT FROM NEW.admission_source AND a.admission_epoch IS NOT DISTINCT FROM NEW.admission_epoch
     AND a.admission_source IS NOT NULL AND a.amount_cents=NEW.amount_cents) THEN
     RAISE EXCEPTION 'automatic purchase must retain saved recharge authority' USING ERRCODE='55000';
    END IF;
   ELSE
    IF NEW.plan_snapshot->'terms'->>'PricingVersion' IS DISTINCT FROM 'v2' THEN
     RAISE EXCEPTION 'enrolled Starter requires frozen v2 terms' USING ERRCODE='55000';
    END IF;
    PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
    END IF;
   ELSE
    IF NEW.request_payload->'PlanTerms'->'PricingV2' IS DISTINCT FROM 'true'::jsonb THEN
     RAISE EXCEPTION 'enrolled Pro requires frozen v2 terms' USING ERRCODE='55000';
    END IF;
    PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
   END IF;
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cohort_signup_entry BEFORE INSERT OR UPDATE ON billing_signup_choice FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_commercial_entry();
CREATE TRIGGER cohort_upgrade_entry BEFORE INSERT OR UPDATE ON billing_pro_upgrade FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_commercial_entry();
CREATE TRIGGER cohort_prepaid_entry BEFORE INSERT OR UPDATE ON billing_prepaid_purchase FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_commercial_entry();
CREATE TRIGGER cohort_recharge_entry BEFORE INSERT OR UPDATE ON billing_prepaid_auto_recharge FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_commercial_entry();
CREATE FUNCTION billing_guard_cohort_checkout() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF NEW.checkout_initializing_at IS NOT NULL AND NEW.checkout_initializing_at IS DISTINCT FROM OLD.checkout_initializing_at
  AND EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=NEW.team_id) THEN
  PERFORM billing_check_cohort_admission(NEW.team_id,NEW.checkout_admission_source,NEW.checkout_admission_epoch);
  IF (NEW.checkout_admission_attempt IS NULL OR NOT NEW.checkout_admission_attempt=ANY(NEW.checkout_pending_attempt_ids))
   AND NOT EXISTS(SELECT 1 FROM billing_signup_choice c WHERE c.team_id=NEW.team_id AND c.state='pending'
    AND c.generation=NEW.checkout_initializing_at AND c.actor_id=NEW.stripe_checkout_actor_id
    AND c.admission_source=NEW.checkout_admission_source AND c.admission_epoch=NEW.checkout_admission_epoch) THEN
   RAISE EXCEPTION 'enrolled checkout requires an exact prepaid admission attempt' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cohort_checkout_entry BEFORE UPDATE ON team_billing_account FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_checkout();

CREATE FUNCTION billing_keep_cohort_free_catalog() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF (billing_new_team_ready() OR EXISTS(SELECT 1 FROM billing_new_team_enrollment))
  AND NOT EXISTS(SELECT 1 FROM billing_plan_version v WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published'
   AND v.kind='free' AND v.max_templates=0 AND v.allowance_period='day_window' AND v.allowance_on_exhausted='pause'
   AND v.allowance_compute_seconds>0 AND v.allowance_compute_vcpu>0 AND v.allowance_compute_memory_gib>0) THEN
  RAISE EXCEPTION 'enrolled teams require a usable published Free catalog; replace atomically' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER cohort_free_catalog_retained AFTER INSERT OR UPDATE OR DELETE ON billing_plan_version
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing_keep_cohort_free_catalog();


ALTER TABLE team_plan_change ADD COLUMN admission_source text, ADD COLUMN admission_epoch uuid;
CREATE FUNCTION billing_guard_cohort_plan_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' AND EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=NEW.team_id) THEN
  IF NEW.terminal_undo_of IS NULL THEN
   IF NEW.target_terms->>'pricing_version' IS DISTINCT FROM 'v2' THEN
    RAISE EXCEPTION 'enrolled plan change requires frozen v2 terms' USING ERRCODE='55000';
   END IF;
   PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
  ELSIF NOT EXISTS(SELECT 1 FROM team_plan_change p WHERE p.id=NEW.terminal_undo_of AND p.team_id=NEW.team_id
    AND p.admission_source IS NOT DISTINCT FROM NEW.admission_source AND p.admission_epoch IS NOT DISTINCT FROM NEW.admission_epoch) THEN
   RAISE EXCEPTION 'terminal undo must retain accepted plan admission' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cohort_plan_change_entry BEFORE INSERT ON team_plan_change
 FOR EACH ROW EXECUTE FUNCTION billing_guard_cohort_plan_change();
CREATE FUNCTION billing_reset_new_team_gate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE g billing_new_team_gate;
BEGIN
 UPDATE billing_new_team_gate SET state='closed',epoch=gen_random_uuid(),released_at=NULL,evidence=NULL WHERE singleton RETURNING * INTO g;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details)
 VALUES(g.epoch,g.region,'raw_switch_reset',COALESCE(NEW.changed_by::text,'database'),COALESCE(NULLIF(btrim(NEW.reason),''),'raw pricing changed'),jsonb_build_object('from',OLD.pricing_version,'to',NEW.pricing_version));
 RETURN NULL;
END $$;
CREATE TRIGGER cohort_gate_raw_switch_reset AFTER UPDATE OF pricing_version ON billing_pricing_switch
 FOR EACH ROW WHEN(OLD.pricing_version IS DISTINCT FROM NEW.pricing_version) EXECUTE FUNCTION billing_reset_new_team_gate();

-- Control and enrollment writes belong to the migration owner, not RPC users
-- or the application service role. Trusted existing creation RPCs retain their
-- explicit service ACL; their internal helper is not independently callable.
DO $$ DECLARE n text; r text; f regprocedure; BEGIN
 FOREACH n IN ARRAY ARRAY['billing_new_team_gate','billing_new_team_enrollment','billing_new_team_gate_event'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',n);
  EXECUTE format('REVOKE ALL ON %I FROM PUBLIC',n);
  FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
   IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON %I FROM %I',n,r); END IF;
  END LOOP;
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
   EXECUTE format('GRANT SELECT ON %I TO service_role',n);
   EXECUTE format('CREATE POLICY new_team_backend_read ON %I TO service_role USING(true)',n);
  END IF;
 END LOOP;
 FOR f IN SELECT p.oid::regprocedure FROM pg_proc p JOIN pg_namespace ns ON ns.oid=p.pronamespace
  WHERE ns.nspname='public' AND p.proname IN ('billing_new_team_evidence_valid','billing_new_team_ready','billing_prepare_new_team_gate','billing_release_new_team_gate','billing_close_new_team_gate','billing_team_pricing_state','billing_lock_team_pricing_state','billing_insert_signup_team','billing_validate_new_team_enrollment','billing_check_cohort_admission','billing_guard_cohort_commercial_entry','billing_guard_cohort_checkout','billing_keep_cohort_free_catalog','billing_guard_cohort_plan_change','billing_reset_new_team_gate','billing_assert_team_pricing_entry') LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
  FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
   IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f,r); END IF;
  END LOOP;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT EXECUTE ON FUNCTION billing_team_pricing_state(uuid),billing_lock_team_pricing_state(uuid),billing_new_team_ready(),billing_assert_team_pricing_entry(uuid) TO service_role;
 END IF;
END $$;
