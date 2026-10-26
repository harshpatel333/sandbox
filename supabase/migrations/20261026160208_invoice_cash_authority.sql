-- Residual invoice cash is distinct from a prepaid purchase or auto-recharge.
-- Freeze the closed usage interval's authority, never today's plan/feature flag.
CREATE TABLE billing_invoice_cash_authority (
 invoice_id text PRIMARY KEY REFERENCES billing_invoice_close(invoice_id),
 team_id uuid NOT NULL,
 period_start timestamptz NOT NULL,
 period_end timestamptz NOT NULL,
 customer_id text NOT NULL,
 subscription_id text NOT NULL,
 policy_version integer NOT NULL CHECK(policy_version=1),
 policy text NOT NULL CHECK(policy IN ('residual_allowed','prepaid_hold','ambiguous_hold')),
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(team_id,period_start,period_end),
 FOREIGN KEY(team_id,period_start,period_end) REFERENCES team_billing_period(team_id,period_start,period_end)
);
ALTER TABLE billing_invoice_cash_authority ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_invoice_cash_authority FROM PUBLIC,anon,authenticated;

CREATE FUNCTION guard_invoice_cash_authority() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'invoice cash authority is immutable';
END $$;
CREATE TRIGGER invoice_cash_authority_immutable BEFORE UPDATE OR DELETE ON billing_invoice_cash_authority
 FOR EACH ROW EXECUTE FUNCTION guard_invoice_cash_authority();
CREATE TRIGGER invoice_cash_authority_no_truncate BEFORE TRUNCATE ON billing_invoice_cash_authority
 FOR EACH STATEMENT EXECUTE FUNCTION guard_invoice_cash_authority();

-- Caller owns the retained close/collection association. SQL is shared with the
-- payment-intent trigger so replicas predating this migration cannot bypass it.
CREATE FUNCTION resolve_invoice_cash_authority(p_invoice text)
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
 -- A paid/unfinished activation can predate assignment fulfillment. Its frozen
 -- prepaid request is a footprint, not consent to charge a residual invoice.
 SELECT COALESCE(jsonb_agg(jsonb_build_object('id',id,'created_at',created_at,'plan_snapshot',plan_snapshot) ORDER BY created_at,id),'[]')
 INTO purchase_evidence FROM billing_prepaid_purchase
 WHERE team_id=c.team_id AND created_at<c.period_end AND (created_at>=c.period_start OR n=0 OR has_gap)
  AND plan_snapshot IS NOT NULL AND plan_snapshot->'terms'->>'kind'='prepaid';
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

-- An old worker persists intent before /pay, including uncertain replay. Stop
-- that transition without rewriting its immutable payment_authorized field.
CREATE FUNCTION guard_invoice_cash_intent() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE authority billing_invoice_cash_authority;
BEGIN
 IF NEW.state='unknown' AND NEW.first_attempt_at IS NOT NULL THEN
  authority:=resolve_invoice_cash_authority(NEW.invoice_id);
  IF authority.policy<>'residual_allowed' OR authority.customer_id<>NEW.customer_id
   OR authority.subscription_id<>NEW.subscription_id THEN
   RAISE EXCEPTION 'invoice residual cash requires review' USING ERRCODE='P0001';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER invoice_cash_intent_guard BEFORE UPDATE OF state,first_attempt_at ON billing_invoice_collection
 FOR EACH ROW EXECUTE FUNCTION guard_invoice_cash_intent();

-- A no-history legacy decision must remain true after freezing. New normal
-- assignments start now; only an attempt to rewrite a frozen interval fails.
CREATE FUNCTION guard_frozen_invoice_plan_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM 1 FROM team WHERE id=NEW.team_id FOR NO KEY UPDATE;
 IF EXISTS(SELECT 1 FROM billing_invoice_cash_authority c WHERE c.team_id=NEW.team_id
  AND NEW.effective_from<c.period_end AND
   ((TG_OP='INSERT' AND COALESCE(NEW.effective_to,'infinity')>c.period_start)
    OR (TG_OP='UPDATE' AND NEW.effective_to<c.period_end AND COALESCE(OLD.effective_to,'infinity')>c.period_start))) THEN
  RAISE EXCEPTION 'plan change would rewrite frozen invoice cash history';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER frozen_invoice_plan_history BEFORE INSERT OR UPDATE ON team_plan_assignment
 FOR EACH ROW EXECUTE FUNCTION guard_frozen_invoice_plan_history();

REVOKE ALL ON FUNCTION guard_invoice_cash_authority(),resolve_invoice_cash_authority(text),guard_invoice_cash_intent(),guard_frozen_invoice_plan_history() FROM PUBLIC,anon,authenticated;
GRANT SELECT,INSERT ON billing_invoice_cash_authority TO service_role;
CREATE POLICY invoice_cash_authority_backend ON billing_invoice_cash_authority TO service_role USING(true) WITH CHECK(true);
GRANT EXECUTE ON FUNCTION resolve_invoice_cash_authority(text) TO service_role;
