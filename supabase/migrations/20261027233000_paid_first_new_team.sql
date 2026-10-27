-- New-team signups start pending: enrolled, with no plan and no compute,
-- until their first paid checkout assigns a plan. Teams already enrolled with
-- a Free assignment keep it and its existing authority checks.
ALTER TABLE billing_new_team_enrollment
 ALTER COLUMN initial_assignment_id DROP NOT NULL,
 ALTER COLUMN initial_free_version_id DROP NOT NULL,
 ADD CONSTRAINT billing_new_team_enrollment_initial_free_pair
  CHECK((initial_assignment_id IS NULL)=(initial_free_version_id IS NULL));

-- Paid-first signup sends every new team to a self-serve subscription, so it
-- applies only while both are sellable. Until then signups keep Free.
CREATE FUNCTION billing_paid_first_sellable() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT count(DISTINCT family)=2 FROM billing_plan_version WHERE family IN ('developer','pro') AND pricing_version='v2' AND state='published'
  AND kind='subscription' AND public AND self_serve AND NOT display_only AND monthly_fee_cents>0;
$$;

CREATE FUNCTION billing_team_pending_payment(p_team uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT EXISTS(SELECT 1 FROM billing_new_team_enrollment e WHERE e.team_id=p_team AND e.initial_assignment_id IS NULL)
  AND NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.team_id=p_team AND p.effective_to IS NULL);
$$;

CREATE OR REPLACE FUNCTION team_sandbox_billing_eligible(p_team_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE
 WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id=p_team_id AND r.released_at IS NULL
 AND (r.expires_at IS NULL OR r.expires_at>now()) AND r.action IN ('create','resume')) THEN false
 WHEN qm_team_managed_billing(p_team_id) THEN true
 WHEN billing_team_pending_payment(p_team_id) THEN false
 WHEN EXISTS(SELECT 1 FROM billing_free_plan(p_team_id)) THEN NOT billing_free_allowance_blocked(p_team_id)
 WHEN NOT COALESCE(f.enforce,false) THEN team_sandbox_billing_eligible_legacy(p_team_id)
 WHEN f.observed_generation IS DISTINCT FROM f.generation
 OR (f.customer_id,f.subscription_id) IS DISTINCT FROM (a.stripe_customer_id,a.stripe_subscription_id) THEN false
 WHEN f.prepaid THEN f.credit_usd>0 AND f.credit_valid_until>now()
 WHEN f.credit_usd>0 AND f.credit_valid_until>now() AND (
 NOT EXISTS(SELECT 1 FROM billing_sponsored_operation WHERE team_id=p_team_id AND state NOT IN ('revoked','expired'))
 OR EXISTS(SELECT 1 FROM billing_usage_entitlement e WHERE e.team_id=p_team_id AND e.sponsorship_id IS NOT NULL AND e.state='issued' AND e.period_start<=now() AND e.period_end>now())) THEN true
 ELSE COALESCE(f.payment_method_ready AND f.collection_ready AND f.provider_valid_until>now()
 AND (billing_funding_fingerprint(p_team_id)='clear' OR f.cash_collected),false) END
 FROM (SELECT p_team_id team_id) t LEFT JOIN billing_funding_account f USING(team_id)
 LEFT JOIN team_billing_account a USING(team_id);
$$;

-- A team waiting for payment has no plan, so it must not fall back to the
-- legacy pay-as-you-go defaults for sandboxes or templates.
CREATE OR REPLACE FUNCTION team_effective_limits(p_team uuid)
RETURNS TABLE(max_concurrent integer, max_vcpu integer, max_memory_mib integer, max_disk_mib integer, max_templates integer)
LANGUAGE sql STABLE AS $$
    WITH base AS (
        SELECT max_template_vcpu, max_template_memory_mib, max_template_disk_mib, max_templates
        FROM billing_plan_version WHERE family = 'payg' AND pricing_version = 'legacy' AND kind = 'legacy_payg' AND state <> 'draft'
        ORDER BY state = 'published' DESC, version DESC LIMIT 1
    ), assigned AS (
        SELECT (limits->>'max_templates')::integer AS max_templates FROM team_plan_assignment
        WHERE team_id=p_team AND effective_to IS NULL
    ), frozen AS (
        SELECT l.max_templates FROM billing_commercial_contract c JOIN billing_contract_limits l ON l.contract_id = c.id
        WHERE c.team_id = p_team AND c.status = 'active' ORDER BY c.starts_at DESC LIMIT 1
    ), pending AS (
        SELECT billing_team_pending_payment(p_team) AS pending
    )
    SELECT CASE WHEN p.pending THEN 0 ELSE t.max_sandboxes END,
        COALESCE(t.max_template_vcpu, b.max_template_vcpu),
        COALESCE(t.max_template_memory_mib, b.max_template_memory_mib),
        COALESCE(t.max_template_disk_mib, b.max_template_disk_mib),
        CASE WHEN p.pending THEN 0 ELSE COALESCE(t.max_templates, (SELECT max_templates FROM assigned), (SELECT max_templates FROM frozen), b.max_templates) END
    FROM team t LEFT JOIN base b ON true CROSS JOIN pending p WHERE t.id = p_team;
$$;

-- Activating pricing v2 for every team must not hand a team waiting for
-- payment a Free plan.
CREATE OR REPLACE FUNCTION pricing_v2_activation_bucket(p_team uuid)
RETURNS TABLE(bucket text, reason text, stripe_customer_id text)
LANGUAGE sql STABLE AS $$
 SELECT CASE
    WHEN x.reason = 'already_assigned' THEN 'assigned'
    WHEN x.reason = 'active_contract' THEN 'contract'
    WHEN x.reason = 'abuse_blocked' THEN 'blocked'
    WHEN x.reason IS NOT NULL THEN 'skipped'
    WHEN x.subscription IS NOT NULL AND lower(COALESCE(x.status, '')) IN ('active','trialing','past_due','unpaid','paused') THEN 'legacy_payg'
    WHEN x.customer IS NOT NULL OR x.subscription IS NOT NULL THEN 'lapsed'
    ELSE 'free' END,
  x.reason, x.customer
 FROM (
  SELECT a.stripe_customer_id AS customer, a.stripe_subscription_id AS subscription, a.stripe_subscription_status AS status,
   CASE
    WHEN EXISTS(SELECT 1 FROM team_plan_assignment pa WHERE pa.team_id = p_team AND pa.effective_to IS NULL) THEN 'already_assigned'
    WHEN qm_team_managed_billing(p_team) THEN 'qm_managed_billing'
    WHEN billing_team_pending_payment(p_team) THEN 'pending_payment'
    WHEN EXISTS(SELECT 1 FROM billing_commercial_contract c WHERE c.team_id = p_team AND c.status='active') THEN 'active_contract'
    WHEN EXISTS(SELECT 1 FROM billing_commercial_contract c WHERE c.team_id = p_team AND c.status='pending') THEN 'pending_contract'
    WHEN a.checkout_initializing_at IS NOT NULL OR a.checkout_may_exist
      OR EXISTS(SELECT 1 FROM billing_signup_choice c WHERE c.team_id=p_team AND c.state='pending' AND c.offer_key<>'free') THEN 'checkout_pending'
    WHEN EXISTS(SELECT 1 FROM billing_yc_operation o WHERE o.team_id = p_team AND o.state <> 'ended') THEN 'yc_operation'
    WHEN EXISTS(SELECT 1 FROM billing_sponsored_operation o WHERE o.team_id = p_team AND o.state NOT IN ('revoked','expired')) THEN 'sponsored_operation'
    WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id = p_team AND r.released_at IS NULL
                AND (r.expires_at IS NULL OR r.expires_at > now()) AND r.action IN ('create','resume')) THEN 'abuse_blocked'
   END AS reason
  FROM (SELECT p_team AS team_id) t LEFT JOIN team_billing_account a USING (team_id)
 ) x;
$$;

CREATE OR REPLACE FUNCTION billing_team_pricing_state(p_team uuid)
RETURNS TABLE(pricing_version text,admission_source text,entry_open boolean,epoch uuid,enrollment_epoch uuid)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
DECLARE raw text; g billing_new_team_gate; e billing_new_team_enrollment; home text; a record;
BEGIN
 SELECT t.home_region INTO STRICT home FROM team t WHERE id=p_team;
 SELECT s.pricing_version INTO STRICT raw FROM billing_pricing_switch s WHERE singleton;
 SELECT * INTO STRICT g FROM billing_new_team_gate WHERE singleton;
 SELECT * INTO e FROM billing_new_team_enrollment n WHERE n.team_id=p_team;
 IF FOUND AND (e.region<>home OR e.region IS DISTINCT FROM g.region
  OR (e.initial_assignment_id IS NOT NULL AND (
   NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.id=e.initial_assignment_id AND p.team_id=p_team AND p.kind='free'
     AND p.source='signup' AND p.plan_version_id=e.initial_free_version_id AND p.effective_from=e.enrolled_at)
   OR NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.team_id=p_team AND p.effective_to IS NULL)))) THEN
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

CREATE OR REPLACE FUNCTION billing_insert_signup_team(p_id uuid,p_name text,p_actor uuid,p_region text,p_source text)
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
  PERFORM 1 FROM billing_plan_version WHERE family IN ('developer','pro','free') AND pricing_version='v2' AND state='published' FOR SHARE;
  IF billing_paid_first_sellable() THEN
   INSERT INTO billing_new_team_enrollment(team_id,region,enrollment_epoch,creator_user_id,source)
    VALUES(p_id,p_region,g.epoch,p_actor,p_source);
  ELSE
   SELECT * INTO STRICT f FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published';
   INSERT INTO billing_new_team_enrollment(team_id,region,enrollment_epoch,initial_assignment_id,initial_free_version_id,creator_user_id,source)
    VALUES(p_id,p_region,g.epoch,gen_random_uuid(),f.id,p_actor,p_source);
  END IF;
 END IF;
 INSERT INTO team(id,name,home_region) VALUES(p_id,p_name,p_region) RETURNING * INTO result;
 RETURN result;
END $$;

CREATE OR REPLACE FUNCTION billing_validate_new_team_enrollment() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF NEW.initial_assignment_id IS NULL THEN
  IF NOT EXISTS(SELECT 1 FROM team t WHERE t.id=NEW.team_id AND t.home_region=NEW.region)
   OR EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.team_id=NEW.team_id AND p.effective_from<=NEW.enrolled_at) THEN
   RAISE EXCEPTION 'pending new-team enrollment must commit with its team and no plan' USING ERRCODE='23514';
  END IF;
 ELSIF NOT EXISTS(SELECT 1 FROM team t JOIN team_plan_assignment p ON p.team_id=t.id
  WHERE t.id=NEW.team_id AND t.home_region=NEW.region AND p.id=NEW.initial_assignment_id
   AND p.plan_version_id=NEW.initial_free_version_id AND p.kind='free' AND p.source='signup'
   AND p.terms->>'pricing_version'='v2' AND p.effective_from=NEW.enrolled_at) THEN
  RAISE EXCEPTION 'new-team enrollment must commit with its actual Free assignment' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;

-- The cohort branch keeps every identity check and only stops assigning Free.
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
  IF e.initial_assignment_id IS NULL THEN RETURN NULL; END IF;
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
 IF NOT billing_paid_first_sellable() THEN
  RAISE EXCEPTION 'published self-serve Developer and Pro subscriptions required' USING ERRCODE='55000';
 END IF;
 IF to_regprocedure('billing_lock_team_pricing_state(uuid)') IS NULL OR to_regprocedure('billing_team_pending_payment(uuid)') IS NULL
  OR NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='team'::regclass AND tgname='team_new_free_plan' AND tgenabled IN ('O','A') AND tgfoid='assign_new_team_free_plan()'::regprocedure) THEN
  RAISE EXCEPTION 'cohort and pending-payment schema required' USING ERRCODE='55000';
 END IF;
 UPDATE billing_new_team_gate SET state='released',evidence=p_evidence,released_at=clock_timestamp() WHERE singleton;
 INSERT INTO billing_new_team_gate_event(epoch,region,action,actor,reason,details) VALUES(g.epoch,g.region,'release',p_actor,p_reason,p_evidence);
 RETURN g.epoch;
END $$;

-- Signups fall back to Free while either subscription is unsellable, so Free
-- stays required only until both are.
CREATE OR REPLACE FUNCTION billing_keep_cohort_free_catalog() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF (billing_new_team_ready() OR EXISTS(SELECT 1 FROM billing_new_team_enrollment)) AND NOT billing_paid_first_sellable()
  AND NOT EXISTS(SELECT 1 FROM billing_plan_version v WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published'
   AND v.kind='free' AND v.max_templates=0 AND v.allowance_period='day_window' AND v.allowance_on_exhausted='pause'
   AND v.allowance_compute_seconds>0 AND v.allowance_compute_vcpu>0 AND v.allowance_compute_memory_gib>0) THEN
  RAISE EXCEPTION 'enrolled teams require a usable published Free catalog or sellable Developer and Pro; replace atomically' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;

DO $$ DECLARE r text; f text; BEGIN
 FOREACH f IN ARRAY ARRAY['billing_paid_first_sellable()','billing_team_pending_payment(uuid)'] LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
  FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
   IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f,r); END IF;
  END LOOP;
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role',f); END IF;
 END LOOP;
END $$;
