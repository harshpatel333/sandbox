-- The remaining trial credit below which the console shows the trial as almost
-- used up. It is configuration and only affects messaging; unset hides that
-- state.
ALTER TABLE billing_trial_policy
 ADD COLUMN low_credit_threshold_usd numeric(12,6) CHECK(low_credit_threshold_usd>0);

-- Each warning lifecycle sends at most one email. A trial awaiting its first
-- plan gets one when the trial ends. A prepaid-funded subscription re-arms with
-- each issued period grant and each issued top-off. Legacy signup trials keep
-- their existing keys.
CREATE OR REPLACE FUNCTION trial_credit_warning_lifecycle(p_team_id uuid)
RETURNS text LANGUAGE sql STABLE AS $$
 SELECT CASE
  WHEN a.id IS NOT NULL THEN 'prepaid:'||md5(concat_ws(',',a.id,
   (SELECT e.id FROM billing_usage_entitlement e WHERE e.team_id=p_team_id AND e.contract_id=a.contract_id AND e.state='issued'
     ORDER BY e.period_start DESC,e.id DESC LIMIT 1),
   (SELECT p.id FROM billing_prepaid_purchase p WHERE p.team_id=p_team_id AND p.credit_state='issued'
     ORDER BY p.created_at DESC,p.id DESC LIMIT 1)))
  WHEN billing_team_awaiting_plan(p_team_id) THEN 'trial:'||grants.key
  ELSE grants.key END
 FROM (SELECT md5(COALESCE(string_agg(id::text,',' ORDER BY id),'')) AS key
   FROM team_credit_grant WHERE team_id=p_team_id AND reason='signup trial credit') grants
 LEFT JOIN team_plan_assignment a ON a.team_id=p_team_id AND a.effective_to IS NULL
  AND a.kind='subscription' AND a.terms->>'funding_policy'='prepaid';
$$;

-- How long a prepaid-funded period may run without its included grant before
-- low-credit messaging stops waiting for it, as when a renewal is unpaid or its
-- grant is withheld. It is configuration.
ALTER TABLE billing_funding_policy
 ADD COLUMN low_credit_grant_grace interval NOT NULL DEFAULT interval '1 hour' CHECK(low_credit_grant_grace>interval '0');

-- A prepaid-funded balance is worth warning about once it was read after the
-- period's included grant and after every top-off paid since: the grant lands
-- after checkout and after each renewal invoice, so a reading in that gap shows
-- only leftover credit. A period whose grant has not landed within the grace
-- is warned on its current balance.
CREATE FUNCTION billing_prepaid_warning_balance_ready(p_team uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
 WITH current AS (
  SELECT a.contract_id, a.effective_from, f.credit_checked_at AS checked
  FROM team_plan_assignment a JOIN billing_funding_account f ON f.team_id=a.team_id
  WHERE a.team_id=p_team AND a.effective_to IS NULL AND a.kind='subscription' AND a.terms->>'funding_policy'='prepaid'
   AND f.observe AND f.observed_generation=f.generation AND f.credit_valid_until>now()
 ), grants AS (
  SELECT e.period_start, e.period_end, e.issued_at AS landed FROM billing_usage_entitlement e, current
  WHERE e.team_id=p_team AND e.contract_id=current.contract_id AND e.state='issued'
  UNION ALL
  SELECT u.period_start, u.period_end, c.observed_at FROM billing_pro_upgrade u
  JOIN billing_provider_credit c ON c.customer_id=u.customer_id AND c.grant_id=u.grant_id, current
  WHERE u.team_id=p_team AND u.contract_id=current.contract_id AND u.state IN ('credited','active')
 )
 SELECT COALESCE(bool_or(
   NOT EXISTS(SELECT 1 FROM grants g WHERE g.landed>=current.checked)
   AND NOT EXISTS(SELECT 1 FROM billing_usage_entitlement e WHERE e.team_id=p_team AND e.state='issued' AND e.issued_at>=current.checked)
   AND NOT EXISTS(SELECT 1 FROM billing_prepaid_purchase p WHERE p.team_id=p_team AND p.payment_state='succeeded'
    AND (p.credit_state<>'issued' OR p.credited_at>=current.checked) AND NOT billing_prepaid_credit_withheld(p))
   AND (EXISTS(SELECT 1 FROM grants g WHERE g.period_start<=now() AND g.period_end>now())
    OR GREATEST(current.effective_from,(SELECT max(g.period_end) FROM grants g WHERE g.period_end<=now()))
       +(SELECT low_credit_grant_grace FROM billing_funding_policy WHERE singleton)<current.checked)),false)
 FROM current;
$$;

DO $$ DECLARE r text; BEGIN
 REVOKE ALL ON FUNCTION billing_prepaid_warning_balance_ready(uuid) FROM PUBLIC;
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON FUNCTION billing_prepaid_warning_balance_ready(uuid) FROM %I',r); END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT EXECUTE ON FUNCTION billing_prepaid_warning_balance_ready(uuid) TO service_role;
 END IF;
END $$;
