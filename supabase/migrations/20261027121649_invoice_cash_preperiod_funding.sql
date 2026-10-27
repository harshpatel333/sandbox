-- Preserve installed migration bytes and immutable frozen invoice decisions.
-- Replace only the prospective resolver; CREATE OR REPLACE retains its ACL.
CREATE OR REPLACE FUNCTION resolve_invoice_cash_authority(p_invoice text)
RETURNS billing_invoice_cash_authority LANGUAGE plpgsql AS $$
DECLARE
 c billing_invoice_close; result billing_invoice_cash_authority;
 a team_plan_assignment; contract_evidence jsonb; cursor_at timestamptz; stop_at timestamptz;
 assignment_evidence jsonb := '[]'; purchase_evidence jsonb := '[]';
 decision text := 'residual_allowed'; has_gap boolean := false; only_legacy_payg boolean := true; n integer := 0;
 customer text; subscription text;
BEGIN
 SELECT * INTO c FROM billing_invoice_close WHERE invoice_id=p_invoice;
 IF NOT FOUND THEN RAISE EXCEPTION 'invoice close authority is missing'; END IF;
 -- Serialize with plan transitions without holding a lock across provider I/O.
 PERFORM 1 FROM team WHERE id=c.team_id FOR SHARE;
 SELECT * INTO result FROM billing_invoice_cash_authority WHERE invoice_id=p_invoice;
 IF FOUND THEN RETURN result; END IF;
 PERFORM 1 FROM team_billing_period WHERE team_id=c.team_id AND period_start=c.period_start
  AND period_end=c.period_end AND finalized_at IS NOT NULL AND period_end<=clock_timestamp();
 IF NOT FOUND OR c.state NOT IN ('verified','released') THEN
  RAISE EXCEPTION 'invoice cash authority requires a closed verified period';
 END IF;
 customer := c.plan->>'customer'; subscription := c.plan->>'subscription';
 IF COALESCE(customer,'')='' OR COALESCE(subscription,'')='' THEN
  RAISE EXCEPTION 'invoice retained provider association is missing';
 END IF;
 -- A verified close retains this association even when a replacement waits.
 IF EXISTS(SELECT 1 FROM billing_invoice_collection j WHERE j.invoice_id=p_invoice
  AND (j.customer_id<>customer OR j.subscription_id<>subscription)) THEN
  RAISE EXCEPTION 'invoice retained provider association differs';
 END IF;
 cursor_at := c.period_start;
 FOR a IN SELECT * FROM team_plan_assignment WHERE team_id=c.team_id
  AND effective_from<c.period_end AND COALESCE(effective_to,'infinity')>c.period_start
  ORDER BY effective_from,id LOOP
  n := n+1;
  IF a.kind<>'legacy_payg' OR a.terms->>'pricing_version'='v2' THEN only_legacy_payg:=false; END IF;
  IF greatest(a.effective_from,c.period_start)>cursor_at THEN has_gap:=true; END IF;
  stop_at:=least(COALESCE(a.effective_to,c.period_end),c.period_end);
  cursor_at:=greatest(cursor_at,stop_at);
  contract_evidence:=NULL;
  IF a.contract_id IS NOT NULL THEN
   SELECT jsonb_build_object('id',id,'team_id',team_id,'customer_id',customer_id,'subscription_id',subscription_id,
    'starts_at',starts_at,'offer_key',offer_key,'charge_amount_cents',charge_amount_cents,'charge_interval_months',charge_interval_months)
   INTO contract_evidence FROM billing_commercial_contract WHERE id=a.contract_id;
  END IF;
  assignment_evidence:=assignment_evidence || jsonb_build_array(jsonb_build_object(
   'id',a.id,'from',greatest(a.effective_from,c.period_start),'to',stop_at,
   'kind',a.kind,'plan_version_id',a.plan_version_id,'terms',a.terms,'contract_id',a.contract_id,'contract',contract_evidence));
  IF a.kind='prepaid' THEN
   decision:='prepaid_hold';
  ELSIF a.kind NOT IN ('legacy_payg','subscription','custom')
    OR (a.terms ? 'kind' AND a.terms->>'kind' IS DISTINCT FROM a.kind)
    OR (a.kind IN ('subscription','custom') AND (a.contract_id IS NULL
     OR contract_evidence->>'team_id' IS DISTINCT FROM c.team_id::text
     OR contract_evidence->>'customer_id' IS DISTINCT FROM customer
     OR contract_evidence->>'subscription_id' IS DISTINCT FROM subscription)) THEN
   IF decision<>'prepaid_hold' THEN decision:='ambiguous_hold'; END IF;
  END IF;
 END LOOP;
 IF n>0 AND cursor_at<c.period_end THEN has_gap:=true; END IF;
 -- A pre-period purchase can still issue credit despite full assignment coverage.
 -- Hold unresolved payments/credits and grants issued during or after the period.
 -- A terminal failure or grant issued before the period adds no new ambiguity
 -- when assignment coverage is complete. Both checkout and auto-recharge rows
 -- are funding authority even without a plan snapshot. Retain the original
 -- snapshot footprint without holding settled legacy rows forever. Frozen decisions stay intact.
 SELECT COALESCE(jsonb_agg(jsonb_build_object('id',id,'created_at',created_at,'plan_snapshot',plan_snapshot) ORDER BY created_at,id),'[]')
 INTO purchase_evidence FROM billing_prepaid_purchase
 WHERE team_id=c.team_id AND created_at<c.period_end
  AND (((created_at>=c.period_start OR n=0 OR has_gap)
    AND plan_snapshot IS NOT NULL AND plan_snapshot->'terms'->>'kind'='prepaid')
   OR (payment_state<>'failed' AND (payment_state<>'succeeded'
    OR credit_state<>'issued' OR credited_at>=c.period_start)));
 IF purchase_evidence<>'[]' THEN decision:='prepaid_hold'; END IF;
 IF EXISTS(SELECT 1 FROM team_plan_assignment WHERE team_id=c.team_id
  AND effective_from<c.period_end AND (terms->>'pricing_version'='v2' OR kind IN ('free','prepaid'))) THEN
  only_legacy_payg:=false;
 END IF;
 IF (has_gap OR n>1) AND NOT only_legacy_payg AND decision<>'prepaid_hold' THEN decision:='ambiguous_hold'; END IF;
 -- No assignment history is normal for established legacy PAYG. Only gaps
 -- inside actual v2 history are ambiguous; never inspect the current plan.
 IF n=0 AND EXISTS(SELECT 1 FROM team_plan_assignment WHERE team_id=c.team_id
  AND effective_from<c.period_end AND (terms->>'pricing_version'='v2' OR kind IN ('free','prepaid'))) THEN
  decision:='ambiguous_hold';
 END IF;
 INSERT INTO billing_invoice_cash_authority(invoice_id,team_id,period_start,period_end,customer_id,subscription_id,policy_version,policy,evidence)
 VALUES(p_invoice,c.team_id,c.period_start,c.period_end,customer,subscription,1,decision,
  jsonb_build_object('assignments',assignment_evidence,'prepaid_requests',purchase_evidence,
   'legacy_no_assignment',n=0 AND decision='residual_allowed','legacy_payg_history',only_legacy_payg,'gap',has_gap,
   'close_customer',customer,'close_subscription',subscription))
 ON CONFLICT(invoice_id) DO NOTHING;
 SELECT * INTO STRICT result FROM billing_invoice_cash_authority WHERE invoice_id=p_invoice;
 RETURN result;
END $$;
