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
