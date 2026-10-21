-- Requires immutable commercial offer assignments. Acceptance and provisioning
-- journals are separate authorities and are deliberately not foreign keys here.
CREATE TABLE billing_contract_provider_binding (
 contract_id uuid PRIMARY KEY REFERENCES billing_commercial_contract(id),
 team_id uuid NOT NULL REFERENCES team(id),
 assignment_id uuid NOT NULL REFERENCES billing_commercial_offer_assignment(id),
 acceptance_id uuid NOT NULL,
 approval_semantic_revision bigint NOT NULL CHECK(approval_semantic_revision>=0),
 merchant text NOT NULL CHECK(merchant LIKE 'acct_%'),
 live_mode boolean NOT NULL,
 api_version text NOT NULL CHECK(btrim(api_version)<>''),
 terms_digest text NOT NULL CHECK(terms_digest ~ '^[0-9a-f]{64}$'),
 provider_terms_digest text NOT NULL CHECK(provider_terms_digest ~ '^[0-9a-f]{64}$'),
 customer_id text NOT NULL CHECK(customer_id LIKE 'cus_%'),
 usage_subscription_id text NOT NULL CHECK(usage_subscription_id LIKE 'sub_%'),
 fee_subscription_id text NOT NULL CHECK(fee_subscription_id LIKE 'sub_%'),
 fee_item_id text NOT NULL CHECK(fee_item_id LIKE 'si_%'),
 anchor timestamptz NOT NULL,
 starts_at timestamptz NOT NULL,
 ends_at timestamptz,
 binding jsonb NOT NULL CHECK(jsonb_typeof(binding)='object'),
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object'),
 verified_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(team_id,contract_id),
 CHECK(usage_subscription_id<>fee_subscription_id),
 CHECK(isfinite(anchor) AND isfinite(starts_at) AND anchor=date_trunc('second',anchor) AND starts_at>=anchor AND (ends_at IS NULL OR isfinite(ends_at) AND ends_at>starts_at))
);
CREATE INDEX billing_provider_pair_usage ON billing_contract_provider_binding(team_id,usage_subscription_id,starts_at DESC);
CREATE INDEX billing_provider_pair_fee ON billing_contract_provider_binding(merchant,live_mode,fee_subscription_id);
ALTER TABLE billing_contract_provider_binding ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_contract_provider_binding FROM PUBLIC;

CREATE FUNCTION guard_billing_contract_provider_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE c billing_commercial_contract; a billing_commercial_offer_assignment; t jsonb;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'provider binding history is immutable'; END IF;
 PERFORM 1 FROM team_billing_account WHERE team_id=NEW.team_id FOR UPDATE;
 SELECT * INTO STRICT c FROM billing_commercial_contract WHERE id=NEW.contract_id AND team_id=NEW.team_id FOR UPDATE;
 SELECT * INTO STRICT a FROM billing_commercial_offer_assignment WHERE id=NEW.assignment_id AND team_id=NEW.team_id;
 t:=NEW.binding->'terms';
 IF c.status<>'pending' OR c.charge_kind<>'subscription'
  OR (c.customer_id,c.subscription_id,c.entitlement_anchor,c.starts_at,c.ends_at)
     IS DISTINCT FROM (NEW.customer_id,NEW.usage_subscription_id,NEW.anchor,NEW.starts_at,NEW.ends_at)
  OR (c.base_price_id,c.charge_amount_cents,c.charge_interval_months,c.currency)
     IS DISTINCT FROM (t->>'fee_price_id',(t->>'fee_amount_cents')::bigint,(t->>'fee_months')::integer,t->>'currency')
  OR NEW.verified_at>clock_timestamp() OR NEW.verified_at<clock_timestamp()-interval '1 minute'
  OR NEW.starts_at IS DISTINCT FROM (SELECT period_start FROM billing_entitlement_period(NEW.anchor,NEW.starts_at)) THEN
  RAISE EXCEPTION 'provider binding differs from pending commercial terms';
 END IF;
 IF (NEW.binding->>'contract_id')::uuid IS DISTINCT FROM NEW.contract_id
  OR NEW.binding->'identity' IS DISTINCT FROM jsonb_build_object(
   'operation_id',NEW.binding->'identity'->>'operation_id','team_id',NEW.team_id,'assignment_id',NEW.assignment_id,
   'acceptance_id',NEW.acceptance_id,'approval_semantic_revision',NEW.approval_semantic_revision,
   'terms_digest',NEW.terms_digest,'provider_terms_digest',NEW.provider_terms_digest,
   'merchant',NEW.merchant,'live',NEW.live_mode,'api_version',NEW.api_version)
  OR (NEW.binding->>'customer_id',NEW.binding->>'usage_subscription_id',NEW.binding->>'fee_subscription_id',NEW.binding->>'fee_item_id')
   IS DISTINCT FROM (NEW.customer_id,NEW.usage_subscription_id,NEW.fee_subscription_id,NEW.fee_item_id)
  OR ((NEW.binding->>'anchor')::timestamptz,(NEW.binding->>'starts_at')::timestamptz,(NEW.binding->>'ends_at')::timestamptz)
   IS DISTINCT FROM (NEW.anchor,NEW.starts_at,NEW.ends_at) THEN
  RAISE EXCEPTION 'provider binding projection differs from immutable evidence';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM team_billing_account WHERE team_id=NEW.team_id AND stripe_customer_id=NEW.customer_id
  AND stripe_subscription_id=NEW.usage_subscription_id AND commercial_billing_anchor=NEW.anchor) THEN
  RAISE EXCEPTION 'provider pair differs from usage account';
 END IF;
 IF EXISTS(SELECT 1 FROM billing_contract_provider_binding b WHERE b.merchant=NEW.merchant AND b.live_mode=NEW.live_mode
  AND (b.fee_subscription_id=NEW.fee_subscription_id OR b.usage_subscription_id=NEW.usage_subscription_id)
  AND ((b.team_id,b.assignment_id,b.customer_id,b.usage_subscription_id,b.fee_subscription_id,b.anchor)
   IS DISTINCT FROM (NEW.team_id,NEW.assignment_id,NEW.customer_id,NEW.usage_subscription_id,NEW.fee_subscription_id,NEW.anchor)
   OR (b.binding->'terms'->'resources',b.binding->'terms'->'rounding') IS DISTINCT FROM (t->'resources',t->'rounding')
   OR tstzrange(b.starts_at,b.ends_at,'[)') && tstzrange(NEW.starts_at,NEW.ends_at,'[)'))) THEN
  RAISE EXCEPTION 'provider pair identity or service windows overlap';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_contract_provider_binding_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_contract_provider_binding
 FOR EACH ROW EXECUTE FUNCTION guard_billing_contract_provider_binding();

-- A paid zero-dollar invoice proves the initial waiver, never cash settlement.
-- Current approval observation is refreshed under the acceptance authority guard.
CREATE TABLE billing_provider_pair_waiver (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 team_id uuid NOT NULL,
 contract_id uuid NOT NULL UNIQUE,
 invoice_id text NOT NULL UNIQUE CHECK(invoice_id LIKE 'in_%'),
 coupon_id text NOT NULL CHECK(btrim(coupon_id)<>''),
 starts_at timestamptz NOT NULL,
 ends_at timestamptz NOT NULL,
 evidence jsonb NOT NULL CHECK(jsonb_typeof(evidence)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 authority_checked_at timestamptz NOT NULL,
 authority_valid_until timestamptz NOT NULL,
 revoked_at timestamptz,
 UNIQUE(team_id,contract_id,id),
 FOREIGN KEY(team_id,contract_id) REFERENCES billing_contract_provider_binding(team_id,contract_id),
 CHECK(ends_at>starts_at AND authority_valid_until>authority_checked_at)
);
ALTER TABLE billing_provider_pair_waiver ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_provider_pair_waiver FROM PUBLIC;

CREATE FUNCTION guard_billing_provider_pair_waiver() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b billing_contract_provider_binding; c billing_commercial_contract;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'paired waiver history is immutable'; END IF;
 IF TG_OP='UPDATE' THEN
  IF (to_jsonb(NEW)-ARRAY['authority_checked_at','authority_valid_until','revoked_at'])
    IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['authority_checked_at','authority_valid_until','revoked_at'])
   OR OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at
   OR NEW.authority_checked_at<OLD.authority_checked_at THEN
   RAISE EXCEPTION 'paired waiver terms and revocation are immutable';
  END IF;
 ELSE
  SELECT * INTO STRICT b FROM billing_contract_provider_binding WHERE contract_id=NEW.contract_id AND team_id=NEW.team_id;
  SELECT * INTO STRICT c FROM billing_commercial_contract WHERE id=NEW.contract_id FOR UPDATE;
  IF c.status<>'pending' OR c.offer_key<>'yc_2026' OR c.charge_amount_cents<>1200000 OR c.charge_interval_months<>12
   OR c.entitlement_cadence<>'scheduled' OR NEW.starts_at<>b.anchor
   OR NEW.ends_at<>billing_anniversary_boundary(b.anchor,12)
   OR (NEW.starts_at,NEW.ends_at) IS DISTINCT FROM (c.starts_at,c.ends_at)
   OR NEW.evidence->>'id' IS DISTINCT FROM NEW.invoice_id
   OR NEW.evidence->>'customer' IS DISTINCT FROM b.customer_id
   OR NEW.evidence#>>'{parent,subscription_details,subscription}' IS DISTINCT FROM b.fee_subscription_id
   OR NEW.evidence->>'status' IS DISTINCT FROM 'paid'
   OR (NEW.evidence->>'amount_paid')::bigint IS DISTINCT FROM 0
   OR (NEW.evidence->>'total')::bigint IS DISTINCT FROM 0 THEN
   RAISE EXCEPTION 'paired waiver differs from verified initial annual fee';
  END IF;
 END IF;
 IF NEW.authority_checked_at>clock_timestamp() OR NEW.authority_checked_at<clock_timestamp()-interval '1 minute'
  OR NEW.authority_valid_until>NEW.authority_checked_at+interval '1 minute'
  OR NEW.revoked_at>clock_timestamp() THEN RAISE EXCEPTION 'paired authority observation is stale or future'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_provider_pair_waiver_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_provider_pair_waiver
 FOR EACH ROW EXECUTE FUNCTION guard_billing_provider_pair_waiver();

CREATE FUNCTION billing_provider_pair_covered(c billing_commercial_contract,s timestamptz,e timestamptz,issuing boolean)
RETURNS boolean LANGUAGE sql VOLATILE AS $$
 SELECT c.charge_kind='subscription' AND c.offer_key='yc_2026' AND EXISTS(
  SELECT 1 FROM billing_provider_pair_waiver w JOIN billing_contract_provider_binding b ON b.contract_id=w.contract_id
  WHERE w.contract_id=c.id AND w.team_id=c.team_id AND b.customer_id=c.customer_id AND b.usage_subscription_id=c.subscription_id
  AND b.anchor=c.entitlement_anchor AND w.starts_at<=s AND w.ends_at>=e
  AND w.revoked_at IS NULL AND w.authority_checked_at<=clock_timestamp() AND w.authority_valid_until>clock_timestamp());
$$;

REVOKE ALL ON FUNCTION guard_billing_contract_provider_binding(),guard_billing_provider_pair_waiver() FROM PUBLIC;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  GRANT SELECT,INSERT ON billing_contract_provider_binding TO service_role;
  CREATE POLICY provider_binding_backend ON billing_contract_provider_binding TO service_role USING(true) WITH CHECK(true);
  GRANT SELECT,INSERT,UPDATE ON billing_provider_pair_waiver TO service_role;
  CREATE POLICY provider_waiver_backend ON billing_provider_pair_waiver TO service_role USING(true) WITH CHECK(true);
 END IF;
END $$;
