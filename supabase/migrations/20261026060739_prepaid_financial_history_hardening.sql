-- Financial history survives broad default grants and later accidental grants.
CREATE FUNCTION guard_billing_prepaid_history_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'prepaid financial history cannot be truncated' USING ERRCODE = '55000';
END $$;
REVOKE ALL ON FUNCTION guard_billing_prepaid_history_truncate() FROM PUBLIC;

DO $$ DECLARE t text; r text; BEGIN
    FOREACH t IN ARRAY ARRAY['billing_prepaid_purchase', 'billing_prepaid_reversal', 'billing_prepaid_reversal_event', 'billing_prepaid_reversal_decision'] LOOP
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON %I FROM %I', t, r);
            END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
            EXECUTE format('GRANT SELECT, INSERT, UPDATE ON %I TO service_role', t);
        END IF;
        EXECUTE format('CREATE TRIGGER prepaid_history_no_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_prepaid_history_truncate()', t);
    END LOOP;
END $$;

-- Starter balance enforcement remains authoritative after Free allowance routing.
CREATE OR REPLACE FUNCTION team_sandbox_billing_eligible(p_team_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE
 WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id=p_team_id AND r.released_at IS NULL
 AND (r.expires_at IS NULL OR r.expires_at>now()) AND r.action IN ('create','resume')) THEN false
 WHEN qm_team_managed_billing(p_team_id) THEN true
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
