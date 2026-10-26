CREATE OR REPLACE FUNCTION end_pro_upgrade() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status='ended' THEN
  UPDATE billing_pro_upgrade SET state='ended' WHERE contract_id=NEW.id;
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION end_pro_upgrade() FROM PUBLIC;

CREATE TABLE billing_pro_released_invoice (
 release_id uuid NOT NULL REFERENCES billing_plan_release(id),
 invoice_id text PRIMARY KEY CHECK(invoice_id<>''),
 contract_id uuid NOT NULL REFERENCES billing_commercial_contract(id),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','recorded','review_required')),
 receipt jsonb,
 payment_reference text,
 revision bigint NOT NULL DEFAULT 1,
 verified_revision bigint NOT NULL DEFAULT 0 CHECK(verified_revision>=0 AND verified_revision<=revision),
 credit_state text NOT NULL DEFAULT 'pending' CHECK(credit_state IN ('pending','covered','review_required')),
 next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_error text,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(state<>'recorded' OR (receipt IS NOT NULL AND payment_reference IS NOT NULL))
);
CREATE INDEX billing_pro_released_invoice_due ON billing_pro_released_invoice(next_check_at) WHERE verified_revision<revision OR state<>'recorded' OR credit_state='pending';
ALTER TABLE billing_pro_released_invoice ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_pro_released_invoice FROM PUBLIC;
DO $$
DECLARE r text;
BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON billing_pro_released_invoice FROM %I',r);
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT SELECT,INSERT,UPDATE ON billing_pro_released_invoice TO service_role;
  CREATE POLICY pro_released_invoice_backend ON billing_pro_released_invoice TO service_role USING(true) WITH CHECK(true);
 END IF;
END $$;

CREATE FUNCTION guard_pro_released_invoice() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'released invoice identity is immutable'; END IF;
 IF TG_OP='UPDATE' AND ((NEW.release_id,NEW.invoice_id,NEW.contract_id) IS DISTINCT FROM (OLD.release_id,OLD.invoice_id,OLD.contract_id)
 OR NEW.revision<OLD.revision OR NEW.verified_revision<OLD.verified_revision
 OR (OLD.receipt IS NOT NULL AND (NEW.receipt,NEW.payment_reference) IS DISTINCT FROM (OLD.receipt,OLD.payment_reference))) THEN
  RAISE EXCEPTION 'released invoice evidence is immutable';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM billing_plan_release r
  JOIN team_plan_change ch ON ch.id=r.plan_change_id AND ch.team_id=r.team_id
  JOIN team_plan_assignment a ON a.id=ch.from_assignment_id AND a.team_id=r.team_id
  JOIN billing_commercial_contract c ON c.id=a.contract_id AND c.team_id=r.team_id
  WHERE r.id=NEW.release_id AND c.id=NEW.contract_id AND c.status='ended'
  AND c.customer_id=r.customer_id AND c.subscription_id=r.subscription_id AND r.yc_operation_id IS NULL) THEN
  RAISE EXCEPTION 'released invoice has no terminal source purchase';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER pro_released_invoice_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_pro_released_invoice
 FOR EACH ROW EXECUTE FUNCTION guard_pro_released_invoice();
REVOKE ALL ON FUNCTION guard_pro_released_invoice() FROM PUBLIC;

-- Existing staging can already have independent fee/usage subscriptions. Keep
-- that authority at this replacement; canonical targets acquire no paired DDL.
DO $compose$
DECLARE paired boolean; missing text;
BEGIN
 SELECT EXISTS(SELECT 1 FROM pg_class WHERE relnamespace='public'::regnamespace
   AND (relname='billing_contract_provider_binding' OR relname LIKE 'billing_provider_pair_%'))
   OR EXISTS(SELECT 1 FROM pg_proc WHERE pronamespace='public'::regnamespace
      AND (proname LIKE 'billing_provider_pair_%'
        OR proname IN ('billing_payment_authorizes_entitlement','billing_payment_allows_activation')))
   OR EXISTS(SELECT 1 FROM pg_attribute WHERE NOT attisdropped AND
       ((attrelid='public.billing_usage_entitlement'::regclass AND attname='paired_waiver_id')
        OR (attrelid='public.billing_entitlement_work'::regclass
         AND attname IN ('paired_invoice_cursor','paired_invoice_period_start')))) INTO paired;
 IF paired THEN
  SELECT string_agg(required.relation || '.' || required.col, ', ') INTO missing
  FROM (VALUES
    ('billing_contract_provider_binding','contract_id','uuid'),
    ('billing_contract_provider_binding','team_id','uuid'),
    ('billing_contract_provider_binding','customer_id','text'),
    ('billing_contract_provider_binding','usage_subscription_id','text'),
    ('billing_contract_provider_binding','fee_subscription_id','text'),
    ('billing_contract_provider_binding','anchor','timestamptz'),
    ('billing_contract_provider_binding','fee_anchor','timestamptz'),
    ('billing_contract_provider_binding','assignment_id','uuid'),
    ('billing_provider_pair_revocation','contract_id','uuid'),
    ('billing_provider_pair_waiver','contract_id','uuid'),
    ('billing_provider_pair_waiver','team_id','uuid'),
    ('billing_provider_pair_waiver','starts_at','timestamptz'),
    ('billing_provider_pair_waiver','ends_at','timestamptz'),
    ('billing_provider_pair_waiver','revoked_at','timestamptz'),
    ('billing_provider_pair_waiver','authority_checked_at','timestamptz'),
    ('billing_provider_pair_waiver','authority_valid_until','timestamptz'),
    ('billing_provider_pair_monthly_payment','payment_id','uuid'),
    ('billing_provider_pair_monthly_payment','contract_id','uuid'),
    ('billing_provider_pair_monthly_payment','team_id','uuid'),
    ('billing_provider_pair_monthly_payment','period_start','timestamptz'),
    ('billing_provider_pair_monthly_payment','period_end','timestamptz'),
    ('billing_commercial_offer_activation','contract_id','uuid'),
    ('billing_commercial_offer_activation','assignment_id','uuid'),
    ('billing_usage_entitlement','paired_waiver_id','uuid'),
    ('billing_entitlement_work','paired_invoice_cursor','text'),
    ('billing_entitlement_work','paired_invoice_period_start','timestamptz')
  ) AS required(relation,col,typ)
  WHERE NOT EXISTS(SELECT 1 FROM pg_attribute a
    WHERE a.attrelid=to_regclass('public.' || required.relation) AND a.attname=required.col
      AND NOT a.attisdropped AND a.atttypid=to_regtype(required.typ));
  -- Supported paired helper bodies match the installed staging contract;
  -- signature-compatible unknown behavior must not silently acquire authority.
  IF missing IS NOT NULL
   OR NOT EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.billing_provider_pair_covered(public.billing_commercial_contract,timestamptz,timestamptz,boolean)') AND prorettype='boolean'::regtype AND NOT prosecdef AND proconfig IS NULL AND prolang=(SELECT oid FROM pg_language WHERE lanname='sql') AND provolatile='v' AND md5(prosrc)='c474ef32696470299b77cc657763cfdd')
   OR NOT EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.billing_payment_authorizes_entitlement(public.billing_commercial_contract,public.billing_contract_payment,timestamptz,timestamptz)') AND prorettype='boolean'::regtype AND NOT prosecdef AND proconfig IS NULL AND prolang=(SELECT oid FROM pg_language WHERE lanname='sql') AND provolatile='s' AND md5(prosrc)='a74f299dc0b9ecfb90bb3e70aebdc0b2') THEN
   RAISE EXCEPTION 'incomplete provider-pair authority schema: %', COALESCE(missing,'helper contract');
  END IF;
 END IF;
 IF paired THEN
  EXECUTE $paired$
CREATE OR REPLACE FUNCTION guard_billing_contract_payment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c billing_commercial_contract; m integer; first_month integer; fee_anchor timestamptz; fee_subscription text;
BEGIN
    IF TG_OP <> 'INSERT' THEN RAISE EXCEPTION 'base payment evidence is immutable'; END IF;
    SELECT * INTO STRICT c FROM billing_commercial_contract WHERE team_id = NEW.team_id AND id = NEW.contract_id FOR SHARE;
    SELECT b.fee_subscription_id,b.fee_anchor INTO fee_subscription,fee_anchor
      FROM public.billing_contract_provider_binding b WHERE b.contract_id=c.id AND b.team_id=c.team_id;
    fee_subscription := COALESCE(fee_subscription,c.subscription_id);
    fee_anchor := COALESCE(fee_anchor,c.entitlement_anchor);
    IF (NEW.customer_id, NEW.subscription_id, NEW.base_price_id, NEW.currency, NEW.base_amount_paid_cents)
        IS DISTINCT FROM (c.customer_id, CASE c.charge_kind WHEN 'subscription' THEN fee_subscription ELSE NULL END, c.base_price_id, c.currency, c.charge_amount_cents) THEN
        RAISE EXCEPTION 'base payment does not match commercial terms';
    END IF;
    IF c.charge_kind = 'upfront' AND ((NEW.service_period_start, NEW.service_period_end) IS DISTINCT FROM (c.starts_at, c.ends_at)
        OR NEW.evidence->'parent' IS DISTINCT FROM 'null'::jsonb) THEN
        RAISE EXCEPTION 'upfront receipt requires standalone invoice and exact contract term';
    END IF;
    IF (c.status = 'ended' AND NOT EXISTS(SELECT 1 FROM billing_pro_released_invoice j
        WHERE j.contract_id=c.id AND j.invoice_id=NEW.invoice_id AND j.receipt=NEW.evidence
        AND j.payment_reference=NEW.payment_reference)) OR NEW.service_period_start < c.starts_at OR NEW.paid_at > clock_timestamp()
        OR NEW.verified_at > clock_timestamp() THEN RAISE EXCEPTION 'invalid base payment period or verification time'; END IF;
    m := (extract(year FROM NEW.service_period_start AT TIME ZONE 'UTC')::integer - extract(year FROM fee_anchor AT TIME ZONE 'UTC')::integer) * 12
        + extract(month FROM NEW.service_period_start AT TIME ZONE 'UTC')::integer - extract(month FROM fee_anchor AT TIME ZONE 'UTC')::integer;
    first_month := (extract(year FROM c.starts_at AT TIME ZONE 'UTC')::integer - extract(year FROM fee_anchor AT TIME ZONE 'UTC')::integer) * 12
        + extract(month FROM c.starts_at AT TIME ZONE 'UTC')::integer - extract(month FROM fee_anchor AT TIME ZONE 'UTC')::integer;
    IF (m - first_month) % c.charge_interval_months <> 0
        OR NEW.service_period_start <> billing_anniversary_boundary(fee_anchor, m)
        OR NEW.service_period_end <> billing_anniversary_boundary(fee_anchor, m + c.charge_interval_months) THEN
        RAISE EXCEPTION 'base payment service period differs from contract cadence';
    END IF;
    RETURN NEW;
END;
$$;
$paired$;
 ELSE
  EXECUTE $canonical$
CREATE OR REPLACE FUNCTION guard_billing_contract_payment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c billing_commercial_contract; m integer; first_month integer;
BEGIN
    IF TG_OP <> 'INSERT' THEN RAISE EXCEPTION 'base payment evidence is immutable'; END IF;
    SELECT * INTO STRICT c FROM billing_commercial_contract WHERE team_id = NEW.team_id AND id = NEW.contract_id FOR SHARE;
    IF (NEW.customer_id, NEW.subscription_id, NEW.base_price_id, NEW.currency, NEW.base_amount_paid_cents)
        IS DISTINCT FROM (c.customer_id, CASE c.charge_kind WHEN 'subscription' THEN c.subscription_id ELSE NULL END, c.base_price_id, c.currency, c.charge_amount_cents) THEN
        RAISE EXCEPTION 'base payment does not match commercial terms';
    END IF;
    IF c.charge_kind = 'upfront' AND ((NEW.service_period_start, NEW.service_period_end) IS DISTINCT FROM (c.starts_at, c.ends_at)
        OR NEW.evidence->'parent' IS DISTINCT FROM 'null'::jsonb) THEN
        RAISE EXCEPTION 'upfront receipt requires standalone invoice and exact contract term';
    END IF;
    IF (c.status = 'ended' AND NOT EXISTS(SELECT 1 FROM billing_pro_released_invoice j
        WHERE j.contract_id=c.id AND j.invoice_id=NEW.invoice_id AND j.receipt=NEW.evidence
        AND j.payment_reference=NEW.payment_reference)) OR NEW.service_period_start < c.starts_at OR NEW.paid_at > clock_timestamp()
        OR NEW.verified_at > clock_timestamp() THEN RAISE EXCEPTION 'invalid base payment period or verification time'; END IF;
    m := (extract(year FROM NEW.service_period_start AT TIME ZONE 'UTC')::integer - extract(year FROM c.entitlement_anchor AT TIME ZONE 'UTC')::integer) * 12
        + extract(month FROM NEW.service_period_start AT TIME ZONE 'UTC')::integer - extract(month FROM c.entitlement_anchor AT TIME ZONE 'UTC')::integer;
    first_month := (extract(year FROM c.starts_at AT TIME ZONE 'UTC')::integer - extract(year FROM c.entitlement_anchor AT TIME ZONE 'UTC')::integer) * 12
        + extract(month FROM c.starts_at AT TIME ZONE 'UTC')::integer - extract(month FROM c.entitlement_anchor AT TIME ZONE 'UTC')::integer;
    IF (m - first_month) % c.charge_interval_months <> 0
        OR NEW.service_period_start <> billing_anniversary_boundary(c.entitlement_anchor, m)
        OR NEW.service_period_end <> billing_anniversary_boundary(c.entitlement_anchor, m + c.charge_interval_months) THEN
        RAISE EXCEPTION 'base payment service period differs from contract cadence';
    END IF;
    RETURN NEW;
END;
$$;
$canonical$;
 END IF;
END $compose$;

REVOKE ALL ON FUNCTION guard_billing_contract_payment() FROM PUBLIC;
