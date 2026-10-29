-- Sponsored teams have no payment method, so at each period roll they depend
-- on the next sponsored grant. That grant is issued only after the roll, once
-- the prior period's closing invoice is released, which leaves a gap with no
-- covering grant. The grace keeps such a team eligible for six hours after
-- its last issued sponsored period ends. It ends early on debt, on withheld
-- unpaid-invoice evidence, on revocation, or when the contract ends with that
-- period. A withheld row written only for a platform failure does not end it:
-- the entitlement worker writes one after five minutes of any ordinary gap.
--
-- The grace is checked before the funding generation, so the generation bump
-- from issuing the next grant cannot pause the team before the funding worker
-- observes it. It lasts until that observation catches up.
--
-- Usage in the grace is billed on the new period's invoice, which the next
-- grant applies to. The invoice close verifies what Stripe applied before it
-- releases anything, so a difference holds the draft for review.

-- A sponsored period whose subscription_cycle invoice was confirmed absent
-- after its provider cycle ended. Recorded once and never rewritten, so a
-- later-arriving invoice cannot change a decision the next grant relied on.
CREATE TABLE billing_sponsored_absent_close (
 contract_id uuid NOT NULL REFERENCES billing_commercial_contract(id),
 team_id uuid NOT NULL,
 period_start timestamptz NOT NULL,
 period_end timestamptz NOT NULL,
 invoice_start timestamptz NOT NULL,
 invoice_end timestamptz NOT NULL,
 customer_id text NOT NULL,
 subscription_id text NOT NULL,
 confirmed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(contract_id,period_end),
 UNIQUE(team_id,period_start,period_end),
 CHECK(period_end>period_start), CHECK(invoice_end>invoice_start)
);
ALTER TABLE billing_sponsored_absent_close ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_sponsored_absent_close FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON billing_sponsored_absent_close FROM %I',r); END IF;
 END LOOP;
END $$;
CREATE TRIGGER billing_sponsored_absent_close_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON billing_sponsored_absent_close
 FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_append_only();

-- A recorded absent period has nothing to close, so it does not hold the
-- periods after it.
CREATE OR REPLACE FUNCTION billing_invoice_period_absent(p_team_id uuid,p_start timestamptz,p_end timestamptz) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM billing_sponsored_absent_close WHERE team_id=p_team_id AND period_start=p_start AND period_end=p_end);
$$;

-- p_observed is whether the funding observation is current; until it is, an
-- issued next grant does not end the grace.
CREATE OR REPLACE FUNCTION billing_sponsored_roll_grace(p_team_id uuid,p_observed boolean) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM billing_sponsored_operation o
  JOIN billing_commercial_contract c ON c.id=o.contract_id AND c.status='active'
  JOIN team_billing_account a ON a.team_id=o.team_id AND a.stripe_customer_id=c.customer_id AND a.stripe_subscription_id=c.subscription_id
  JOIN billing_usage_entitlement e ON e.team_id=o.team_id AND e.contract_id=c.id AND e.sponsorship_id=o.id AND e.state='issued'
  WHERE o.team_id=p_team_id AND o.state='active' AND o.revoked_at IS NULL
   AND e.period_end<=now() AND e.period_end>now()-interval '6 hours'
   AND (c.ends_at IS NULL OR c.ends_at>e.period_end)
   AND (NOT p_observed OR NOT EXISTS(SELECT 1 FROM billing_usage_entitlement n WHERE n.team_id=o.team_id AND n.contract_id=c.id AND n.period_start=e.period_end AND n.state='issued'))
   AND NOT EXISTS(SELECT 1 FROM billing_entitlement_debt d WHERE d.contract_id=c.id AND d.period_start>=e.period_end)
   AND NOT EXISTS(SELECT 1 FROM billing_entitlement_withheld w WHERE w.contract_id=c.id AND w.period_start>=e.period_end AND w.evidence ? 'unpaid_invoice'));
$$;

CREATE OR REPLACE FUNCTION team_sandbox_billing_eligible(p_team_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE
 WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id=p_team_id AND r.released_at IS NULL
 AND (r.expires_at IS NULL OR r.expires_at>now()) AND r.action IN ('create','resume')) THEN false
 WHEN qm_team_managed_billing(p_team_id) THEN true
 WHEN billing_team_awaiting_plan(p_team_id) THEN billing_team_trial_active(p_team_id)
 WHEN EXISTS(SELECT 1 FROM billing_free_plan(p_team_id)) THEN NOT billing_free_allowance_blocked(p_team_id)
 WHEN NOT COALESCE(f.enforce,false) THEN team_sandbox_billing_eligible_legacy(p_team_id)
 WHEN billing_sponsored_roll_grace(p_team_id,f.observed_generation IS NOT DISTINCT FROM f.generation) THEN true
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
