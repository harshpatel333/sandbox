-- A subscription plan can fund usage like prepaid credit: its included grant
-- and top-ups are spent first and sandboxes pause when they run out. The fee
-- and any overshoot still collect on the period-end invoice.
ALTER TABLE billing_plan_version
 ADD COLUMN funding_policy text NOT NULL DEFAULT 'postpaid' CHECK (funding_policy IN ('prepaid','postpaid')),
 ADD COLUMN topoff_amount_cents bigint CHECK (topoff_amount_cents BETWEEN 500 AND 1000000 AND topoff_amount_cents%500=0),
 ADD COLUMN topoff_threshold_cents bigint CHECK (topoff_threshold_cents BETWEEN 500 AND 1000000 AND topoff_threshold_cents%500=0),
 ADD COLUMN topoff_default_enabled boolean,
 ADD CHECK (funding_policy='postpaid' OR kind='subscription'),
 ADD CHECK ((topoff_amount_cents IS NULL) = (topoff_threshold_cents IS NULL)
  AND (topoff_amount_cents IS NULL) = (topoff_default_enabled IS NULL)),
 ADD CHECK (topoff_amount_cents IS NULL OR funding_policy='prepaid');

-- Prepaid kinds and prepaid-funded subscriptions share the credit gate and
-- auto-recharge. Terms are frozen, so a catalog edit never moves a team.
CREATE FUNCTION billing_assignment_prepaid_funded(a team_plan_assignment) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT a.kind='prepaid' OR (a.kind='subscription' AND a.terms->>'funding_policy' IS NOT DISTINCT FROM 'prepaid')
$$;
REVOKE ALL ON FUNCTION billing_assignment_prepaid_funded(team_plan_assignment) FROM PUBLIC;
DO $$ DECLARE role_name text; BEGIN
 FOREACH role_name IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('REVOKE ALL ON FUNCTION billing_assignment_prepaid_funded(team_plan_assignment) FROM %I',role_name);
   IF role_name='service_role' THEN
    GRANT EXECUTE ON FUNCTION billing_assignment_prepaid_funded(team_plan_assignment) TO service_role;
   END IF;
  END IF;
 END LOOP;
END $$;

CREATE OR REPLACE FUNCTION prepaid_funding_follow_assignment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT billing_assignment_prepaid_funded(NEW) THEN
  UPDATE billing_funding_account SET prepaid=false WHERE team_id=NEW.team_id AND prepaid;
  UPDATE billing_prepaid_auto_recharge SET enabled=false,disabled_reason='team left prepaid',disabled_at=clock_timestamp(),notified_at=NULL
   WHERE team_id=NEW.team_id AND enabled;
 END IF;
 RETURN NULL;
END $$;

-- A prepaid-funded subscription bills its fee and overshoot at period end, so
-- settled top-ups no longer hold its invoice; unsettled ones still do.
CREATE OR REPLACE FUNCTION resolve_invoice_cash_authority(p_invoice text)
RETURNS billing_invoice_cash_authority LANGUAGE plpgsql AS $$
DECLARE
 c billing_invoice_close; result billing_invoice_cash_authority;
 a team_plan_assignment; contract_evidence jsonb; cursor_at timestamptz; stop_at timestamptz;
 assignment_evidence jsonb := '[]'; purchase_evidence jsonb := '[]';
 decision text := 'residual_allowed'; has_gap boolean := false; only_legacy_payg boolean := true; n integer := 0;
 customer text; subscription text; funded_subscription boolean := true;
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
  IF a.kind<>'subscription' OR NOT billing_assignment_prepaid_funded(a) THEN funded_subscription:=false; END IF;
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
 funded_subscription := funded_subscription AND n=1 AND NOT has_gap;
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
   OR (billing_prepaid_funding_unresolved(billing_prepaid_purchase,c.period_start,c.period_end)
    AND NOT (funded_subscription AND payment_state='succeeded' AND credit_state='issued')));
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
   'legacy_no_assignment',n=0 AND decision='residual_allowed','legacy_payg_history',only_legacy_payg,'gap',has_gap,'prepaid_funded_subscription',funded_subscription,
   'close_customer',customer,'close_subscription',subscription))
 ON CONFLICT(invoice_id) DO NOTHING;
 SELECT * INTO STRICT result FROM billing_invoice_cash_authority WHERE invoice_id=p_invoice;
 RETURN result;
END $$;

-- Checkouts whose Auto Topoff choice was applied. A retried settlement of any
-- of them, even an older one, never overwrites a later choice.
CREATE TABLE billing_checkout_topoff (
 session_id text PRIMARY KEY CHECK (btrim(session_id)<>''),
 team_id uuid NOT NULL REFERENCES team(id) ON DELETE CASCADE,
 enabled boolean NOT NULL,
 applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE billing_checkout_topoff ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_checkout_topoff FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN EXECUTE format('REVOKE ALL ON billing_checkout_topoff FROM %I',r); END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT SELECT, INSERT ON billing_checkout_topoff TO service_role;
  CREATE POLICY backend_access ON billing_checkout_topoff TO service_role USING (true) WITH CHECK (true);
 END IF;
END $$;
