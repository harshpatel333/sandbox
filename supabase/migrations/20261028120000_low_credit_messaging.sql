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

-- A prepaid-funded balance is only worth warning about once the period's
-- included grant has been issued and observed, and no top-off or grant has
-- been issued since the reading: the grant lands after checkout and after each
-- renewal invoice, so a reading in that gap shows only leftover credit.
CREATE FUNCTION billing_prepaid_warning_balance_ready(p_team uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(bool_or(f.observe AND f.observed_generation=f.generation AND f.credit_valid_until>now()
  AND (EXISTS(SELECT 1 FROM billing_usage_entitlement e WHERE e.team_id=p_team AND e.contract_id=a.contract_id AND e.state='issued'
        AND e.period_start<=now() AND e.period_end>now())
   OR EXISTS(SELECT 1 FROM billing_pro_upgrade u JOIN billing_provider_credit c ON c.customer_id=u.customer_id AND c.grant_id=u.grant_id
        WHERE u.team_id=p_team AND u.contract_id=a.contract_id AND u.state IN ('credited','active')
        AND u.period_start<=now() AND u.period_end>now() AND c.observed_at<f.credit_checked_at))
  AND NOT EXISTS(SELECT 1 FROM billing_usage_entitlement e WHERE e.team_id=p_team AND e.state='issued' AND e.issued_at>=f.credit_checked_at)
  AND NOT EXISTS(SELECT 1 FROM billing_prepaid_purchase p WHERE p.team_id=p_team AND p.credit_state='issued' AND p.credited_at>=f.credit_checked_at)),false)
 FROM team_plan_assignment a JOIN billing_funding_account f ON f.team_id=a.team_id
 WHERE a.team_id=p_team AND a.effective_to IS NULL AND a.kind='subscription' AND a.terms->>'funding_policy'='prepaid';
$$;
