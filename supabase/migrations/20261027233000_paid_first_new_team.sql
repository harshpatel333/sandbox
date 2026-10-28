-- New-team signups start on a no-card trial: enrolled with no plan until their
-- first paid checkout assigns one. Teams already enrolled with a Free
-- assignment keep it and its existing authority checks.
ALTER TABLE billing_new_team_enrollment
 ALTER COLUMN initial_assignment_id DROP NOT NULL,
 ALTER COLUMN initial_free_version_id DROP NOT NULL,
 ADD CONSTRAINT billing_new_team_enrollment_initial_free_pair
  CHECK((initial_assignment_id IS NULL)=(initial_free_version_id IS NULL));

-- The trial a new team gets before it pays. It is configuration: signups stay
-- on Free until every value is set, and the values are frozen into the team's
-- credit grant and limits when its trial starts.
CREATE TABLE billing_trial_policy (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 credit_usd numeric(12,6) CHECK(credit_usd>0),
 max_sandboxes integer CHECK(max_sandboxes>0),
 max_vcpu integer CHECK(max_vcpu>0),
 max_memory_mib integer CHECK(max_memory_mib>0),
 max_disk_mib integer CHECK(max_disk_mib>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO billing_trial_policy DEFAULT VALUES;
ALTER TABLE billing_trial_policy ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_trial_policy FROM PUBLIC;

CREATE FUNCTION billing_trial_configured() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT COALESCE((SELECT credit_usd IS NOT NULL AND max_sandboxes IS NOT NULL AND max_vcpu IS NOT NULL
  AND max_memory_mib IS NOT NULL AND max_disk_mib IS NOT NULL FROM billing_trial_policy WHERE singleton),false);
$$;

-- Paid-first signup sends every new team to a self-serve subscription, so it
-- applies only while checkout can sell both, by the same rule checkout uses.
-- Until then signups keep Free.
CREATE FUNCTION billing_paid_first_sellable() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT count(DISTINCT v.family)=2 FROM billing_plan_version v WHERE v.family IN ('developer','pro') AND v.pricing_version='v2' AND v.state='published'
  AND v.kind='subscription' AND v.public AND v.self_serve AND NOT v.display_only AND v.monthly_fee_cents>0
  AND v.matching_bps IS NULL AND billing_plan_version_monthly_credit(v) IS NOT NULL;
$$;

-- Paid offers are the legacy offer rows plus every offer a v2 subscription
-- version has sold, retired ones included so accepted purchases still settle
-- and renew. Only the active pricing model's offers get billing_plan rows.
CREATE FUNCTION billing_paid_offer_keys() RETURNS SETOF text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT offer_key FROM billing_plan
 UNION
 SELECT v.family||'_monthly' FROM billing_plan_version v
  WHERE v.pricing_version='v2' AND v.kind='subscription' AND v.state<>'draft' AND v.monthly_fee_cents IS NOT NULL
 UNION
 SELECT v.family||'_annual' FROM billing_plan_version v
  WHERE v.pricing_version='v2' AND v.kind='subscription' AND v.state<>'draft' AND v.annual_fee_cents IS NOT NULL;
$$;

CREATE OR REPLACE FUNCTION billing_plan_offer(p_offer text) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM billing_paid_offer_keys() k WHERE k = p_offer);
$$;

CREATE FUNCTION billing_team_awaiting_plan(p_team uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT EXISTS(SELECT 1 FROM billing_new_team_enrollment e WHERE e.team_id=p_team AND e.initial_assignment_id IS NULL)
  AND NOT EXISTS(SELECT 1 FROM team_plan_assignment p WHERE p.team_id=p_team AND p.effective_to IS NULL);
$$;


-- A team awaiting its plan has compute only while its trial credit lasts; the
-- trial eligibility worker settles exhaustion as for the signup trial.
CREATE FUNCTION billing_team_trial_active(p_team uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT billing_team_awaiting_plan(p_team)
  AND EXISTS(SELECT 1 FROM team_credit_grant g WHERE g.team_id=p_team AND g.reason='signup trial credit' AND g.remaining_usd>0
   AND (g.expires_at IS NULL OR g.expires_at>now()))
  AND NOT EXISTS(SELECT 1 FROM team_billing_account a WHERE a.team_id=p_team AND a.trial_ended_at IS NOT NULL)
  AND COALESCE((SELECT c.eligible FROM team_trial_eligibility_cache c WHERE c.team_id=p_team),true);
$$;

-- Plan checkout prepares its subscription before payment, so a team still
-- awaiting its plan can hold an active subscription it has not paid for. Only
-- a plan ends the trial; the subscription alone neither ends nor extends it.
CREATE OR REPLACE FUNCTION get_team_trial_balance(p_team_id uuid)
RETURNS TABLE(grant_usd numeric, consumed_usd numeric, remaining_usd numeric, state text, eligible boolean)
LANGUAGE sql STABLE AS $$
WITH account AS (
  SELECT trial_ended_at, stripe_subscription_id,
    CASE WHEN billing_team_awaiting_plan(p_team_id) THEN NULL ELSE stripe_subscription_status END AS stripe_subscription_status
  FROM team_billing_account WHERE team_id = p_team_id
), historical_grants AS (
  SELECT COUNT(*)::int AS count
  FROM team_credit_grant
  WHERE team_id = p_team_id AND reason = 'signup trial credit'
), grants AS (
  SELECT COALESCE(SUM(amount_usd) FILTER (WHERE expires_at IS NULL OR expires_at > now()), 0)::numeric AS amount, COUNT(*)::int AS count,
         COUNT(*) FILTER (WHERE expires_at IS NULL OR expires_at > now())::int AS active_count,
         COALESCE(MIN(created_at) FILTER (WHERE expires_at IS NULL OR expires_at > now()), now()) AS started,
         MAX(expires_at) FILTER (WHERE expires_at IS NOT NULL) AS expires_end
  FROM team_credit_grant WHERE team_id = p_team_id AND reason = 'signup trial credit'
), bounds AS (
  SELECT COALESCE((SELECT trial_ended_at FROM account),
                  CASE WHEN grants.count > 0 AND grants.active_count = 0
                       THEN COALESCE(grants.expires_end, now()) ELSE now() END) AS period_end
  FROM grants
), compute_usage AS (
  SELECT COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, bounds.period_end), bounds.period_end) - GREATEST(i.started_at, grants.started))), 0) * i.vcpu_count), 0)::numeric AS cpu,
         COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM (LEAST(COALESCE(i.ended_at, bounds.period_end), bounds.period_end) - GREATEST(i.started_at, grants.started))), 0) * i.memory_mib / 1024.0), 0)::numeric AS memory
  FROM sandbox_compute_billing_interval i, bounds, grants
  WHERE i.team_id = p_team_id AND i.started_at < bounds.period_end AND COALESCE(i.ended_at, bounds.period_end) > grants.started
), usage AS (
  SELECT compute_usage.cpu,
         compute_usage.memory,
         (SELECT billable_storage_mib_seconds(p_team_id, grants.started, bounds.period_end) / 1024.0 FROM bounds) AS storage
  FROM compute_usage, grants
), ranked_rates AS (
  SELECT r.*, row_number() OVER (PARTITION BY r.resource, r.unit ORDER BY r.effective_from DESC, r.created_at DESC, r.id DESC) AS rate_rank
  FROM pricing_rate r JOIN pricing_plan pp ON pp.key = r.plan_key AND pp.active
  WHERE r.plan_key = COALESCE((SELECT plan_key FROM team_pricing_plan tpp JOIN pricing_plan tppp ON tppp.key = tpp.plan_key AND tppp.active WHERE tpp.team_id = p_team_id AND tpp.effective_from <= now() AND (tpp.effective_to IS NULL OR tpp.effective_to > now()) ORDER BY tpp.effective_from DESC LIMIT 1), 'payg')
    AND r.unit = 'second' AND r.effective_from <= now() AND (r.effective_to IS NULL OR r.effective_to > now())
), rates AS (
  SELECT COALESCE(MAX(price_usd) FILTER (WHERE resource = 'vcpu'), 0)::numeric cpu,
         COALESCE(MAX(price_usd) FILTER (WHERE resource = 'memory_gib'), 0)::numeric memory,
         COALESCE(MAX(price_usd) FILTER (WHERE resource = 'storage_gib'), 0)::numeric storage
  FROM ranked_rates WHERE rate_rank = 1
), calc AS (
  SELECT grants.amount, round((usage.cpu * rates.cpu + usage.memory * rates.memory + CASE WHEN storage_billing_activated(p_team_id) THEN usage.storage * rates.storage ELSE 0 END)::numeric, 6) consumed,
         grants.count, grants.active_count, historical_grants.count AS historical_count,
         account.trial_ended_at, account.stripe_subscription_id, account.stripe_subscription_status
  FROM grants, historical_grants, usage, rates LEFT JOIN account ON true
)
SELECT amount, consumed,
  CASE WHEN trial_ended_at IS NOT NULL OR stripe_subscription_status IS NOT NULL AND lower(stripe_subscription_status) IN ('active','trialing','past_due') OR (historical_count > 0 AND active_count = 0)
       THEN 0::numeric ELSE round(GREATEST(amount - consumed, 0)::numeric, 6) END,
  CASE WHEN trial_ended_at IS NOT NULL OR stripe_subscription_status IS NOT NULL AND lower(stripe_subscription_status) IN ('active','trialing','past_due') THEN 'ended_by_billing_activation'
       WHEN historical_count = 0 THEN 'no_grant'
       WHEN active_count = 0 THEN 'expired'
       WHEN amount - consumed <= 0 THEN 'exhausted' ELSE 'active' END,
  CASE WHEN trial_ended_at IS NOT NULL OR stripe_subscription_status IS NOT NULL AND lower(stripe_subscription_status) IN ('active','trialing','past_due') THEN lower(COALESCE(stripe_subscription_status, '')) IN ('active','trialing','past_due')
       WHEN historical_count = 0 THEN false
       WHEN active_count = 0 THEN false
       -- Eligibility describes whether active credit may still be consumed;
       -- the warning path separately requires a meaningful usage sample.
       ELSE amount - consumed > 0 END
FROM calc;
$$;

CREATE FUNCTION billing_team_entry_state(p_team uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT CASE
  WHEN NOT billing_team_awaiting_plan(p_team) THEN 'active'
  WHEN billing_team_trial_active(p_team) THEN 'trial'
  WHEN EXISTS(SELECT 1 FROM billing_pro_upgrade_setup s JOIN billing_pro_upgrade u ON u.id=s.upgrade_id
   WHERE u.team_id=p_team AND u.state NOT IN ('expired','ended') AND s.state='open') THEN 'payment_failed'
  ELSE 'trial_ended' END;
$$;

CREATE OR REPLACE FUNCTION team_sandbox_billing_eligible(p_team_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE
 WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id=p_team_id AND r.released_at IS NULL
 AND (r.expires_at IS NULL OR r.expires_at>now()) AND r.action IN ('create','resume')) THEN false
 WHEN qm_team_managed_billing(p_team_id) THEN true
 WHEN billing_team_awaiting_plan(p_team_id) THEN billing_team_trial_active(p_team_id)
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

-- A team awaiting its plan has no plan to fall back on: its trial limits are
-- on the team, and it builds no templates.
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
        SELECT billing_team_awaiting_plan(p_team) AS pending
    )
    SELECT t.max_sandboxes,
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
    WHEN billing_team_awaiting_plan(p_team) THEN 'awaiting_plan'
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
  PERFORM 1 FROM billing_trial_policy WHERE singleton FOR SHARE;
  IF billing_paid_first_sellable() AND billing_trial_configured() THEN
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

-- A team awaiting its plan claims the configured trial through the same user
-- and device fences as the signup trial, instead of the fixed legacy grant.
CREATE OR REPLACE FUNCTION claim_team_signup_trial_with_device(p_team_id uuid, p_user_id uuid)
RETURNS TABLE(outcome text, reason text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE v_reason text; v_fingerprint text; v_outcome text; v_claim_reason text; v_actor uuid;
    v_provenance team_signup_trial_provenance; v_trial billing_trial_policy; v_cohort_trial boolean;
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
    SELECT * INTO v_trial FROM billing_trial_policy WHERE singleton;
    v_cohort_trial := billing_team_awaiting_plan(p_team_id);
    IF v_cohort_trial AND (v_trial.credit_usd IS NULL OR v_trial.max_sandboxes IS NULL OR v_trial.max_vcpu IS NULL
        OR v_trial.max_memory_mib IS NULL OR v_trial.max_disk_mib IS NULL) THEN
        v_reason := 'trial_not_configured';
    ELSIF billing_pricing_version() = 'v2' OR (NOT v_cohort_trial AND EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=p_team_id)) THEN
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
    IF v_outcome = 'granted' AND v_cohort_trial THEN
        UPDATE team_credit_grant g SET amount_usd = v_trial.credit_usd, remaining_usd = v_trial.credit_usd
            WHERE g.team_id = p_team_id AND g.reason = 'signup trial credit';
        PERFORM apply_assignment_limits(p_team_id, jsonb_build_object('max_concurrent', v_trial.max_sandboxes,
            'max_vcpu', v_trial.max_vcpu, 'max_memory_mib', v_trial.max_memory_mib, 'max_disk_mib', v_trial.max_disk_mib));
    END IF;
    IF v_outcome = 'granted' THEN
        SELECT e.fingerprint INTO v_fingerprint FROM promotion_signup_device_evidence e WHERE e.user_id = p_user_id;
        INSERT INTO promotion_device_grant(promotion, user_id, team_id, fingerprint)
            VALUES('signup', p_user_id, p_team_id, v_fingerprint) ON CONFLICT DO NOTHING;
    END IF;
    RETURN QUERY SELECT v_outcome, v_claim_reason;
END;
$$;

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
  IF e.initial_assignment_id IS NULL THEN
   PERFORM apply_assignment_limits(NEW.id,jsonb_build_object('max_concurrent',0));
   RETURN NULL;
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
 IF NOT (billing_paid_first_sellable() AND billing_trial_configured()) AND NOT EXISTS(SELECT 1 FROM billing_plan_version v
   WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published' AND v.kind='free' AND v.max_templates=0
    AND v.allowance_period='day_window' AND v.allowance_on_exhausted='pause' AND v.allowance_compute_seconds>0
    AND v.allowance_compute_vcpu>0 AND v.allowance_compute_memory_gib>0) THEN
  RAISE EXCEPTION 'a usable Free catalog, or sellable Developer and Pro with a configured trial, required' USING ERRCODE='55000';
 END IF;
 IF to_regprocedure('billing_lock_team_pricing_state(uuid)') IS NULL OR to_regprocedure('billing_team_awaiting_plan(uuid)') IS NULL
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
 IF (billing_new_team_ready() OR EXISTS(SELECT 1 FROM billing_new_team_enrollment)) AND NOT (billing_paid_first_sellable() AND billing_trial_configured())
  AND NOT EXISTS(SELECT 1 FROM billing_plan_version v WHERE v.family='free' AND v.pricing_version='v2' AND v.state='published'
   AND v.kind='free' AND v.max_templates=0 AND v.allowance_period='day_window' AND v.allowance_on_exhausted='pause'
   AND v.allowance_compute_seconds>0 AND v.allowance_compute_vcpu>0 AND v.allowance_compute_memory_gib>0) THEN
  RAISE EXCEPTION 'enrolled teams require a usable published Free catalog or sellable Developer and Pro; replace atomically' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER cohort_trial_policy_retained AFTER UPDATE ON billing_trial_policy
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION billing_keep_cohort_free_catalog();

DO $$ DECLARE r text; f text; BEGIN
 FOREACH f IN ARRAY ARRAY['billing_paid_offer_keys()','billing_paid_first_sellable()','billing_trial_configured()','billing_team_awaiting_plan(uuid)','billing_team_trial_active(uuid)','billing_team_entry_state(uuid)'] LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
  FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
   IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I',f,r); END IF;
  END LOOP;
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role',f); END IF;
 END LOOP;
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON billing_trial_policy FROM %I',r); END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT SELECT ON billing_trial_policy TO service_role;
  CREATE POLICY billing_trial_policy_backend ON billing_trial_policy FOR SELECT TO service_role USING(true);
 END IF;
END $$;
