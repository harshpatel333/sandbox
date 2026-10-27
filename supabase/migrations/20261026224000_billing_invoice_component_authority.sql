-- The historical usage policy is deliberately unchanged. A verified paid Pro
-- bridge can separately prove its next fee and that no protected usage needs cash.
CREATE TABLE billing_pro_invoice_cash_authority (
 invoice_id text PRIMARY KEY REFERENCES billing_invoice_close(invoice_id),
 settlement_hash text NOT NULL CHECK(length(settlement_hash)=64),
 team_id uuid NOT NULL REFERENCES team(id),
 customer_id text NOT NULL,
 subscription_id text NOT NULL,
 upgrade_id uuid NOT NULL REFERENCES billing_pro_upgrade(id),
 contract_id uuid NOT NULL REFERENCES billing_commercial_contract(id),
 assignment_id uuid NOT NULL REFERENCES team_plan_assignment(id),
 protected_cents bigint NOT NULL CHECK(protected_cents>=0),
 credit_applied_cents bigint NOT NULL CHECK(credit_applied_cents>=protected_cents),
 fee_cents bigint NOT NULL CHECK(fee_cents>0),
 usage_net_cents bigint NOT NULL CHECK(usage_net_cents>=0),
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object' AND evidence->>'version'='1'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE billing_pro_invoice_cash_authority ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_pro_invoice_cash_authority FROM PUBLIC;
CREATE TRIGGER pro_invoice_cash_immutable BEFORE UPDATE OR DELETE ON billing_pro_invoice_cash_authority
 FOR EACH ROW EXECUTE FUNCTION guard_invoice_cash_authority();
CREATE TRIGGER pro_invoice_cash_no_truncate BEFORE TRUNCATE ON billing_pro_invoice_cash_authority
 FOR EACH STATEMENT EXECUTE FUNCTION guard_invoice_cash_authority();

CREATE FUNCTION pro_invoice_cash_source_valid(p_invoice text,p_hash text) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT EXISTS(
 SELECT 1 FROM billing_pro_invoice_cash_authority proof
 JOIN billing_invoice_close close ON close.invoice_id=proof.invoice_id
 JOIN billing_pro_upgrade u ON u.id=proof.upgrade_id
 JOIN billing_commercial_contract contract ON contract.id=proof.contract_id
 JOIN team_plan_assignment assignment ON assignment.id=proof.assignment_id
 WHERE proof.invoice_id=p_invoice AND proof.settlement_hash=p_hash
 AND close.state IN ('verified','released') AND billing_invoice_close_matches(close.team_id,close.period_start,close.period_end)
 AND close.team_id=proof.team_id AND close.plan->>'customer'=proof.customer_id AND close.plan->>'subscription'=proof.subscription_id
 AND close.plan->>'credit_version'='1' AND proof.evidence->'plan'=close.plan AND proof.evidence->'finalized_evidence'=close.finalized_evidence
 AND (close.plan->>'base_cents')::bigint=proof.fee_cents
 AND (close.plan->>'base_period_start')::bigint=extract(epoch FROM close.period_end)::bigint
 AND (close.finalized_evidence->>'credit_applied_cents')::bigint=proof.credit_applied_cents
 AND (close.finalized_evidence->>'net_cents')::bigint=proof.usage_net_cents
 AND (close.finalized_evidence->'invoice'->>'total')::numeric=proof.fee_cents::numeric+proof.usage_net_cents
 AND u.team_id=proof.team_id AND u.customer_id=proof.customer_id AND u.subscription_id=proof.subscription_id
 AND u.contract_id=contract.id AND u.period_start=close.period_start AND u.period_end=close.period_end
 AND u.state IN ('active','ended') AND u.payment_evidence IS NOT NULL AND NOT u.payment_review_required
 AND contract.team_id=proof.team_id AND contract.customer_id=proof.customer_id AND contract.subscription_id=proof.subscription_id
 AND contract.starts_at=close.period_end AND contract.charge_interval_months=1 AND contract.charge_amount_cents=proof.fee_cents
 AND contract.base_price_id=u.request_payload->>'BasePriceID'
 AND contract.charge_amount_cents=(u.request_payload->'PlanTerms'->>'Amount')::bigint
 AND contract.entitlement_amount_cents=(u.request_payload->'PlanTerms'->>'Credit')::bigint
 AND contract.entitlement_anchor=u.anchor
 AND (contract.ends_at IS NULL OR contract.ends_at>=to_timestamp((close.plan->>'base_period_end')::bigint))
 AND assignment.team_id=proof.team_id AND assignment.contract_id=contract.id AND assignment.kind='subscription'
 AND assignment.source='checkout' AND assignment.idempotency_key='checkout:'||contract.id::text
 AND assignment.plan_version_id=(u.request_payload->'PlanTerms'->>'VersionID')::uuid
 AND assignment.effective_from>=close.period_start AND assignment.effective_from<close.period_end
 AND COALESCE(assignment.effective_to,'infinity')>=close.period_end
 AND (proof.evidence->>'paid_assignment_from')::timestamptz=assignment.effective_from
 AND NOT EXISTS(SELECT 1 FROM team_plan_assignment other WHERE other.team_id=proof.team_id AND other.id<>assignment.id
  AND other.effective_from<close.period_end AND COALESCE(other.effective_to,'infinity')>assignment.effective_from))
$$;

-- Validate the retained financial association at the SQL barrier as well as in
-- the new collector. An old collector cannot turn an unrelated proof into cash.
CREATE OR REPLACE FUNCTION guard_invoice_cash_intent() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE authority billing_invoice_cash_authority;
BEGIN
 IF NEW.state='unknown' AND NEW.first_attempt_at IS NOT NULL THEN
  authority:=resolve_invoice_cash_authority(NEW.invoice_id);
  IF authority.customer_id<>NEW.customer_id OR authority.subscription_id<>NEW.subscription_id
   OR (authority.policy<>'residual_allowed' AND NOT pro_invoice_cash_source_valid(NEW.invoice_id,NEW.settlement_hash)) THEN
   RAISE EXCEPTION 'invoice residual cash requires review' USING ERRCODE='P0001';
  END IF;
 END IF;
 RETURN NEW;
END $$;

REVOKE ALL ON FUNCTION pro_invoice_cash_source_valid(text,text) FROM PUBLIC;
DO $$
DECLARE role_name text;
BEGIN
 FOREACH role_name IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('REVOKE ALL ON billing_pro_invoice_cash_authority FROM %I',role_name);
   EXECUTE format('REVOKE ALL ON FUNCTION pro_invoice_cash_source_valid(text,text) FROM %I',role_name);
   IF role_name='service_role' THEN
    GRANT SELECT,INSERT ON billing_pro_invoice_cash_authority TO service_role;
    CREATE POLICY pro_invoice_cash_backend ON billing_pro_invoice_cash_authority TO service_role USING(true) WITH CHECK(true);
    GRANT EXECUTE ON FUNCTION pro_invoice_cash_source_valid(text,text) TO service_role;
   END IF;
  END IF;
 END LOOP;
END $$;
