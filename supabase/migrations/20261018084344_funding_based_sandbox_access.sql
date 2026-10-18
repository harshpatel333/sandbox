-- Observe funding before opting a cohort into the new access rule. Plan and
-- promotion histories remain separate from authority to consume resources.
CREATE TABLE billing_funding_account (
 team_id uuid PRIMARY KEY REFERENCES team(id) ON DELETE CASCADE,
 observe boolean NOT NULL DEFAULT false,
 enforce boolean NOT NULL DEFAULT false,
 generation bigint NOT NULL DEFAULT 0,
 next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_token uuid, lease_until timestamptz,
 customer_id text, subscription_id text,
 observed_generation bigint,
 credit_usd numeric NOT NULL DEFAULT 0 CHECK(credit_usd>=0),
 credit_valid_until timestamptz,
 credit_evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
 payment_method_id text,
 payment_method_ready boolean NOT NULL DEFAULT false,
 collection_ready boolean NOT NULL DEFAULT false,
 cash_charge_id text,
 cash_collected boolean NOT NULL DEFAULT false,
 charge_cursor text,
 provider_valid_until timestamptz,
 provider_evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
 checked_at timestamptz,
 provider_checked_at timestamptz,
 credit_checked_at timestamptz,
 last_error text,
 failures integer NOT NULL DEFAULT 0,
 CHECK(NOT enforce OR observe)
);
CREATE INDEX billing_funding_due ON billing_funding_account(next_check_at,team_id) WHERE observe;
CREATE INDEX billing_funding_expiry ON billing_funding_account(provider_valid_until) WHERE enforce;
ALTER TABLE billing_funding_account ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_funding_account FROM PUBLIC;

-- Increase the cohort budget only after measuring refresh latency at capacity.
CREATE TABLE billing_funding_policy (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 max_observed_teams integer NOT NULL DEFAULT 4 CHECK(max_observed_teams BETWEEN 1 AND 10000)
);
INSERT INTO billing_funding_policy DEFAULT VALUES;
ALTER TABLE billing_funding_policy ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_funding_policy FROM PUBLIC;
CREATE FUNCTION guard_funding_cohort() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE budget integer;
BEGIN
 IF NEW.observe AND (TG_OP='INSERT' OR NOT OLD.observe) THEN
  SELECT max_observed_teams INTO STRICT budget FROM billing_funding_policy WHERE singleton FOR UPDATE;
  IF (SELECT count(*) FROM billing_funding_account WHERE observe)>=budget THEN
   RAISE EXCEPTION 'funding observation cohort exceeds measured capacity budget';
  END IF;
  NEW.generation:=NEW.generation+1;
  NEW.next_check_at:=clock_timestamp();
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER funding_cohort_guard BEFORE INSERT OR UPDATE OF observe ON billing_funding_account
 FOR EACH ROW EXECUTE FUNCTION guard_funding_cohort();

CREATE TABLE billing_plan_release (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 team_id uuid NOT NULL REFERENCES team(id),
 actor_id uuid NOT NULL,
 previous_offer text,
 customer_id text,
 subscription_id text,
 yc_operation_id uuid,
 previous_choice jsonb NOT NULL,
 previous_account jsonb NOT NULL,
 previous_yc jsonb,
 provider_status text NOT NULL CHECK(provider_status IN ('no_attempt','canceled','incomplete_expired')),
 provider_evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
 released_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX billing_plan_release_identity ON billing_plan_release(team_id,COALESCE(subscription_id,''),COALESCE(yc_operation_id::text,''));
ALTER TABLE billing_plan_release ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_plan_release FROM PUBLIC;

CREATE FUNCTION guard_billing_plan_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE a team_billing_account; o billing_yc_operation;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'plan release history is immutable'; END IF;
 SELECT * INTO STRICT a FROM team_billing_account WHERE team_id=NEW.team_id FOR UPDATE;
 IF (a.stripe_customer_id,a.stripe_subscription_id) IS DISTINCT FROM (NEW.customer_id,NEW.subscription_id)
 OR NEW.previous_account IS DISTINCT FROM to_jsonb(a)
 OR NOT EXISTS(SELECT 1 FROM billing_signup_choice c WHERE c.team_id=NEW.team_id AND to_jsonb(c)=NEW.previous_choice)
 OR EXISTS(SELECT 1 FROM billing_commercial_contract c WHERE c.team_id=NEW.team_id AND c.status<>'ended') THEN
  RAISE EXCEPTION 'plan release account preconditions changed';
 END IF;
 IF (a.checkout_initializing_at IS NOT NULL OR a.checkout_may_exist) AND NOT COALESCE(
  NEW.previous_choice->>'offer_key' IN ('pro_monthly','pro_annual') AND NEW.previous_choice->>'state'='committed'
  AND cardinality(a.checkout_pending_attempt_ids)=0
  AND NEW.provider_evidence->'checkout'->>'status'='complete'
  AND NEW.provider_evidence->'checkout'->>'mode'='subscription'
  AND NEW.provider_evidence->'checkout'->>'id'=a.checkout_session_id
  AND NEW.provider_evidence->'checkout'->>'id'=NEW.previous_choice->>'session_id'
  AND NEW.provider_evidence->'checkout'->>'customer'=NEW.customer_id
  AND NEW.provider_evidence->'checkout'->>'subscription'=NEW.subscription_id
  AND NEW.provider_evidence->'checkout'->>'client_reference_id'=NEW.team_id::text
  AND (NEW.provider_evidence->'checkout'->'metadata'->>'checkout_generation')::timestamptz=a.checkout_initializing_at
  AND (NEW.previous_choice->>'generation')::timestamptz=a.checkout_initializing_at
  AND NEW.provider_evidence->'checkout'->'metadata'->>'commercial_offer'=NEW.previous_choice->>'offer_key'
  AND NEW.provider_evidence->'checkout'->'metadata'->>'activation_user_id'=NEW.previous_choice->>'actor_id'
  AND NEW.previous_choice->'request_payload'->>'CustomerID'=NEW.customer_id
  AND NEW.previous_choice->'request_payload'->>'IdempotencyKey'=a.checkout_request_key,false) THEN
   RAISE EXCEPTION 'plan release has unresolved checkout attempts';
 END IF;
 IF NEW.subscription_id IS NULL THEN
  IF NEW.provider_status<>'no_attempt' THEN RAISE EXCEPTION 'missing subscription requires no-attempt proof'; END IF;
 ELSIF NEW.provider_status NOT IN ('canceled','incomplete_expired') OR NEW.provider_evidence='{}'::jsonb THEN
  RAISE EXCEPTION 'plan release requires terminal provider evidence';
 END IF;
 SELECT * INTO o FROM billing_yc_operation WHERE team_id=NEW.team_id FOR UPDATE;
 IF FOUND AND NOT billing_yc_released(NEW.team_id) THEN
  IF NEW.yc_operation_id IS DISTINCT FROM o.id OR NEW.previous_yc IS DISTINCT FROM to_jsonb(o)
   OR o.state<>'ended' OR (o.subscription_id IS NULL AND (o.provider_attempt_at IS NOT NULL OR o.revoked_at IS NULL)) THEN
   RAISE EXCEPTION 'YC release requires reconciled terminal operation';
  END IF;
 ELSIF NEW.yc_operation_id IS NOT NULL OR NEW.previous_yc IS NOT NULL THEN
  RAISE EXCEPTION 'YC release identity missing';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_plan_release_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_plan_release
 FOR EACH ROW EXECUTE FUNCTION guard_billing_plan_release();

CREATE FUNCTION billing_yc_released(p_team uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM billing_yc_operation o JOIN billing_plan_release r
 ON r.team_id=o.team_id AND r.yc_operation_id=o.id
 AND r.customer_id IS NOT DISTINCT FROM o.customer_id
 AND r.subscription_id IS NOT DISTINCT FROM o.subscription_id
 WHERE o.team_id=p_team AND o.state='ended');
$$;

CREATE FUNCTION release_billing_claim_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE yc_claim_team_intent i SET released_at=NEW.released_at
 FROM billing_yc_operation o WHERE o.id=NEW.yc_operation_id AND i.team_id=NEW.team_id
 AND i.claim_id::text=o.request->>'claim_id' AND i.released_at IS NULL;
 RETURN NULL;
END $$;
CREATE TRIGGER billing_plan_release_claim AFTER INSERT ON billing_plan_release
 FOR EACH ROW EXECUTE FUNCTION release_billing_claim_intent();

CREATE FUNCTION guard_released_billing_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='billing_yc_operation' THEN
  IF EXISTS(SELECT 1 FROM billing_plan_release WHERE team_id=OLD.team_id AND yc_operation_id=OLD.id)
   AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'released YC operation is immutable'; END IF;
 ELSE
  IF NEW.status<>'ended' AND EXISTS(SELECT 1 FROM billing_plan_release r
   WHERE r.team_id=NEW.team_id AND r.customer_id=NEW.customer_id AND r.subscription_id=NEW.subscription_id) THEN
   RAISE EXCEPTION 'released subscription cannot reactivate a contract';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER funding_yc_release_guard BEFORE UPDATE ON billing_yc_operation
 FOR EACH ROW EXECUTE FUNCTION guard_released_billing_identity();
CREATE TRIGGER funding_contract_release_guard BEFORE INSERT OR UPDATE ON billing_commercial_contract
 FOR EACH ROW EXECUTE FUNCTION guard_released_billing_identity();

CREATE OR REPLACE FUNCTION guard_yc_billing_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM billing_plan_release r WHERE r.team_id=NEW.team_id
 AND r.customer_id=NEW.stripe_customer_id AND r.subscription_id=NEW.stripe_subscription_id)
 AND (NEW.stripe_subscription_id IS DISTINCT FROM OLD.stripe_subscription_id
 OR NEW.stripe_subscription_status NOT IN ('canceled','incomplete_expired')) THEN
  RAISE EXCEPTION 'released subscription cannot restore billing access';
 END IF;
 IF EXISTS(SELECT 1 FROM billing_plan_release WHERE team_id=NEW.team_id) AND
 (NEW.stripe_activation_credit_reserved_at IS NOT NULL OR NEW.stripe_activation_credit_grant_id IS NOT NULL
 OR NEW.stripe_activation_credit_granted_at IS NOT NULL) AND
 (NEW.stripe_activation_credit_reserved_at,NEW.stripe_activation_credit_grant_id,NEW.stripe_activation_credit_granted_at)
 IS DISTINCT FROM (OLD.stripe_activation_credit_reserved_at,OLD.stripe_activation_credit_grant_id,OLD.stripe_activation_credit_granted_at) THEN
  RAISE EXCEPTION 'plan transition cannot reissue activation promotions';
 END IF;
 IF EXISTS(SELECT 1 FROM billing_yc_operation WHERE team_id=NEW.team_id) AND NOT billing_yc_released(NEW.team_id) THEN
  IF NEW.checkout_initializing_at IS NOT NULL OR NEW.stripe_activation_credit_reserved_at IS NOT NULL
   OR NEW.stripe_activation_credit_grant_id IS NOT NULL OR NEW.stripe_activation_credit_granted_at IS NOT NULL THEN
   RAISE EXCEPTION 'YC approval excludes self-service checkout and PAYG activation promotions';
  END IF;
  IF NEW.trial_ended_at IS DISTINCT FROM OLD.trial_ended_at AND NOT EXISTS(
   SELECT 1 FROM billing_commercial_contract c WHERE c.team_id=NEW.team_id AND c.status='active'
   AND billing_yc_covered(c,c.starts_at,c.ends_at,false)) THEN
   RAISE EXCEPTION 'YC trial completion requires usable covered access';
  END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION schedule_billing_funding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t uuid;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF TG_TABLE_NAME='team_billing_account' THEN
   IF (NEW.stripe_customer_id,NEW.stripe_subscription_id,NEW.stripe_subscription_status,NEW.commercial_billing_anchor,NEW.trial_ended_at,NEW.stripe_activation_credit_grant_id,NEW.stripe_activation_credit_granted_at)
    IS NOT DISTINCT FROM (OLD.stripe_customer_id,OLD.stripe_subscription_id,OLD.stripe_subscription_status,OLD.commercial_billing_anchor,OLD.trial_ended_at,OLD.stripe_activation_credit_grant_id,OLD.stripe_activation_credit_granted_at) THEN RETURN NULL; END IF;
  ELSIF TG_TABLE_NAME='billing_signup_choice' THEN
   IF (NEW.request_payload,NEW.subscription_id,NEW.offer_key) IS NOT DISTINCT FROM (OLD.request_payload,OLD.subscription_id,OLD.offer_key) THEN RETURN NULL; END IF;
  ELSIF TG_TABLE_NAME='team_billing_period' THEN
   IF (NEW.finalized_at,NEW.status) IS NOT DISTINCT FROM (OLD.finalized_at,OLD.status) THEN RETURN NULL; END IF;
  ELSIF TG_TABLE_NAME='billing_invoice_close' THEN
   IF (NEW.plan,NEW.state) IS NOT DISTINCT FROM (OLD.plan,OLD.state) THEN RETURN NULL; END IF;
  ELSIF (to_jsonb(NEW)-ARRAY['updated_at','last_attempt_at','last_error']) IS NOT DISTINCT FROM (to_jsonb(OLD)-ARRAY['updated_at','last_attempt_at','last_error']) THEN RETURN NULL;
  END IF;
 END IF;
 IF TG_TABLE_NAME='team' THEN t:=NEW.id; ELSE t:=COALESCE(NEW.team_id,OLD.team_id); END IF;
 IF NOT EXISTS(SELECT 1 FROM team WHERE id=t) THEN RETURN NULL; END IF;
 INSERT INTO billing_funding_account(team_id) VALUES(t)
 ON CONFLICT(team_id) DO UPDATE SET generation=billing_funding_account.generation+1,
 next_check_at=LEAST(billing_funding_account.next_check_at,clock_timestamp());
 RETURN NULL;
END $$;
CREATE TRIGGER funding_team AFTER INSERT ON team FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_mapping AFTER INSERT OR UPDATE OF stripe_customer_id,stripe_subscription_id,stripe_subscription_status,commercial_billing_anchor,trial_ended_at,stripe_activation_credit_grant_id,stripe_activation_credit_granted_at ON team_billing_account
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_grant AFTER INSERT OR UPDATE OR DELETE ON team_credit_grant
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_period AFTER INSERT OR UPDATE OF finalized_at,status ON team_billing_period
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_enrollment AFTER INSERT OR UPDATE OF customer_id,subscription_id,adjustment_price_id,adjustment_event_name,adjustment_meter_id,enrolled_at OR DELETE ON billing_invoice_account
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_ledger AFTER INSERT OR UPDATE OR DELETE ON team_credit_ledger
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_correction AFTER INSERT OR UPDATE OR DELETE ON billing_export_correction
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_storage AFTER INSERT ON team_storage_billing_activation
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_team_flag AFTER INSERT OR UPDATE OR DELETE ON team_feature_flag
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_team_pricing AFTER INSERT OR UPDATE OR DELETE ON team_pricing_plan
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_choice AFTER UPDATE OF request_payload,subscription_id,offer_key ON billing_signup_choice
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE TRIGGER funding_close AFTER INSERT OR UPDATE OF plan,state ON billing_invoice_close
 FOR EACH ROW EXECUTE FUNCTION schedule_billing_funding();
CREATE FUNCTION schedule_funding_catalog_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE billing_funding_account SET generation=generation+1,next_check_at=LEAST(next_check_at,clock_timestamp()) WHERE observe;
 RETURN NULL;
END $$;
CREATE TRIGGER funding_catalog AFTER INSERT OR UPDATE OR DELETE ON pricing_rate
 FOR EACH STATEMENT EXECUTE FUNCTION schedule_funding_catalog_change();
CREATE TRIGGER funding_global_flag AFTER INSERT OR UPDATE OR DELETE ON feature_flag
 FOR EACH STATEMENT EXECUTE FUNCTION schedule_funding_catalog_change();

-- Reuse is a device ownership fact, not a generic denied-promotion marker.
CREATE INDEX IF NOT EXISTS funding_provenance_creator ON team_signup_trial_provenance(creator_user_id,team_id);
CREATE FUNCTION billing_funding_fingerprint(p_team uuid) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN e.user_id IS NULL OR o.user_id IS NULL THEN 'unknown'
 WHEN o.user_id<>p.creator_user_id OR EXISTS(SELECT 1 FROM promotion_device_grant g
 WHERE g.fingerprint=e.fingerprint AND g.user_id<>p.creator_user_id AND g.promotion IN ('signup','stripe')) THEN 'reused'
 ELSE 'clear' END
 FROM (SELECT (SELECT creator_user_id FROM team_signup_trial_provenance WHERE team_id=p_team) creator_user_id) p
 LEFT JOIN promotion_signup_device_evidence e ON e.user_id=p.creator_user_id
 LEFT JOIN promotion_device_owner o ON o.fingerprint=e.fingerprint;
$$;

ALTER FUNCTION team_sandbox_billing_eligible(uuid) RENAME TO team_sandbox_billing_eligible_legacy;
CREATE FUNCTION team_sandbox_billing_eligible(p_team_id uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT CASE
 WHEN EXISTS(SELECT 1 FROM abuse_restrictions r WHERE r.subject_team_id=p_team_id AND r.released_at IS NULL
 AND (r.expires_at IS NULL OR r.expires_at>now()) AND r.action IN ('create','resume')) THEN false
 WHEN qm_team_managed_billing(p_team_id) THEN true
 WHEN NOT COALESCE(f.enforce,false) THEN team_sandbox_billing_eligible_legacy(p_team_id)
 WHEN f.observed_generation IS DISTINCT FROM f.generation
 OR (f.customer_id,f.subscription_id) IS DISTINCT FROM (a.stripe_customer_id,a.stripe_subscription_id) THEN false
 WHEN f.credit_usd>0 AND f.credit_valid_until>now() THEN true
 ELSE COALESCE(f.payment_method_ready AND f.collection_ready AND f.provider_valid_until>now()
 AND (billing_funding_fingerprint(p_team_id)='clear' OR f.cash_collected),false) END
 FROM (SELECT p_team_id team_id) t LEFT JOIN billing_funding_account f USING(team_id)
 LEFT JOIN team_billing_account a USING(team_id);
$$;

INSERT INTO billing_funding_account(team_id) SELECT id FROM team ON CONFLICT DO NOTHING;
DO $$ DECLARE r text; f regprocedure; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON billing_funding_account,billing_plan_release,billing_funding_policy FROM %I',r);
   IF r='service_role' THEN
    GRANT SELECT,INSERT,UPDATE ON billing_funding_account TO service_role;
    GRANT SELECT,INSERT ON billing_plan_release TO service_role;
    GRANT SELECT,UPDATE ON billing_funding_policy TO service_role;
    CREATE POLICY funding_backend ON billing_funding_account TO service_role USING(true) WITH CHECK(true);
    CREATE POLICY release_backend ON billing_plan_release TO service_role USING(true) WITH CHECK(true);
    CREATE POLICY funding_policy_backend ON billing_funding_policy TO service_role USING(true) WITH CHECK(true);
   END IF;
  END IF;
 END LOOP;
 FOREACH f IN ARRAY ARRAY['guard_billing_plan_release()'::regprocedure,'billing_yc_released(uuid)'::regprocedure,
 'schedule_billing_funding()'::regprocedure,'release_billing_claim_intent()'::regprocedure,'guard_funding_cohort()'::regprocedure,
 'guard_released_billing_identity()'::regprocedure,'schedule_funding_catalog_change()'::regprocedure,'billing_funding_fingerprint(uuid)'::regprocedure,
 'team_sandbox_billing_eligible(uuid)'::regprocedure,'team_sandbox_billing_eligible_legacy(uuid)'::regprocedure] LOOP
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f);
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role',f); END IF;
 END LOOP;
END $$;
