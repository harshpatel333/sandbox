-- Versioned plan terms. A family (starter, pro, enterprise) has at most one
-- published version; publishing a draft retires the previous one. Published
-- and retired versions are immutable apart from retirement, so contracts and
-- checkouts that froze a version's terms can always be traced back to it.
-- Credit schedules use the timed-grant vocabulary: each grant starts at a
-- month offset from the contract anchor, repeats (NULL means indefinitely)
-- every N months and expires N months after it is issued.
CREATE FUNCTION billing_credit_schedule_valid(p jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE g jsonb; k text;
BEGIN
    IF p IS NULL OR jsonb_typeof(p) <> 'array' OR jsonb_array_length(p) > 24 THEN RETURN false; END IF;
    FOR g IN SELECT value FROM jsonb_array_elements(p) LOOP
        IF jsonb_typeof(g) <> 'object' THEN RETURN false; END IF;
        FOR k IN SELECT jsonb_object_keys(g) LOOP
            IF k NOT IN ('start_months', 'repeat', 'every_months', 'amount_cents', 'expires_after_months') THEN RETURN false; END IF;
        END LOOP;
        IF jsonb_typeof(g->'start_months') IS DISTINCT FROM 'number' OR jsonb_typeof(g->'every_months') IS DISTINCT FROM 'number'
            OR jsonb_typeof(g->'amount_cents') IS DISTINCT FROM 'number' OR jsonb_typeof(g->'expires_after_months') IS DISTINCT FROM 'number'
            OR jsonb_typeof(COALESCE(g->'repeat', 'null'::jsonb)) NOT IN ('number', 'null') THEN
            RETURN false;
        END IF;
        -- Digits only: jsonb keeps 1.0 as written, which Go cannot read as an integer.
        IF (g->>'start_months') !~ '^[0-9]{1,9}$' OR (g->>'every_months') !~ '^[0-9]{1,9}$'
            OR (g->>'expires_after_months') !~ '^[0-9]{1,9}$' OR (g->>'amount_cents') !~ '^[0-9]{1,12}$'
            OR (jsonb_typeof(g->'repeat') = 'number' AND (g->>'repeat') !~ '^[0-9]{1,9}$') THEN
            RETURN false;
        END IF;
        IF (g->>'start_months')::bigint NOT BETWEEN 0 AND 120
            OR (g->>'every_months')::bigint NOT BETWEEN 1 AND 12
            OR (g->>'expires_after_months')::bigint NOT BETWEEN 1 AND 12
            OR (g->>'amount_cents')::bigint NOT BETWEEN 1 AND 100000000
            OR (jsonb_typeof(g->'repeat') = 'number' AND (g->>'repeat')::bigint NOT BETWEEN 1 AND 120) THEN
            RETURN false;
        END IF;
    END LOOP;
    RETURN true;
END $$;

CREATE TABLE billing_plan_version (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Underscores allow customer-specific families such as custom_example;
    -- a family never ends like an offer key's interval.
    family text NOT NULL CHECK (family ~ '^[a-z][a-z0-9]*(_[a-z0-9]+)*$' AND length(family) <= 63
        AND family !~ '_(monthly|annual)$'),
    version integer NOT NULL CHECK (version > 0),
    state text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'published', 'retired')),
    display_name text NOT NULL CHECK (btrim(display_name) <> '' AND length(display_name) <= 80),
    tagline text NOT NULL DEFAULT '' CHECK (length(tagline) <= 200),
    features text[] NOT NULL DEFAULT '{}' CHECK (cardinality(features) <= 20),
    sort_order integer NOT NULL DEFAULT 0 CHECK (sort_order BETWEEN 0 AND 1000),
    -- Display-only families (Custom/Enterprise) are shown but never sold.
    display_only boolean NOT NULL DEFAULT false,
    self_serve boolean NOT NULL DEFAULT false,
    -- Public families are the cards shown at signup; customer-specific deals
    -- are their own non-public families.
    public boolean NOT NULL DEFAULT false,
    monthly_fee_cents bigint CHECK (monthly_fee_cents > 0),
    annual_fee_cents bigint CHECK (annual_fee_cents > 0),
    credit_schedule jsonb NOT NULL DEFAULT '[]' CHECK (billing_credit_schedule_valid(credit_schedule)),
    matching_bps integer CHECK (matching_bps BETWEEN 1 AND 10000),
    cap_cents bigint CHECK (cap_cents > 0),
    max_sandboxes integer CHECK (max_sandboxes > 0),
    max_template_vcpu integer CHECK (max_template_vcpu > 0),
    max_template_memory_mib integer CHECK (max_template_memory_mib > 0),
    max_template_disk_mib integer CHECK (max_template_disk_mib > 0),
    created_by uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_by uuid,
    published_at timestamptz,
    retired_by uuid,
    retired_at timestamptz,
    UNIQUE (family, version),
    CHECK ((matching_bps IS NULL) = (cap_cents IS NULL)),
    CHECK (NOT display_only OR (monthly_fee_cents IS NULL AND annual_fee_cents IS NULL AND NOT self_serve
        AND credit_schedule = '[]'::jsonb AND matching_bps IS NULL)),
    -- A version without fees is a staff-assigned arrangement of credits and/or
    -- matching. Fee-bearing families need catalog offer keys, which have no
    -- underscore in the family, and limits for the plan they sell.
    CHECK (display_only OR monthly_fee_cents IS NOT NULL OR annual_fee_cents IS NOT NULL
        OR credit_schedule <> '[]'::jsonb OR matching_bps IS NOT NULL),
    CHECK ((monthly_fee_cents IS NULL AND annual_fee_cents IS NULL)
        OR (max_sandboxes IS NOT NULL AND family ~ '^[a-z][a-z0-9]*$')),
    -- Fee-less arrangements are staff-assigned and keyed by a bare family, so
    -- their family always has an underscore and is never a public card.
    CHECK (display_only OR monthly_fee_cents IS NOT NULL OR annual_fee_cents IS NOT NULL
        OR (position('_' in family) > 0 AND NOT public)),
    CHECK (NOT self_serve OR monthly_fee_cents IS NOT NULL OR annual_fee_cents IS NOT NULL),
    CHECK ((state = 'draft') = (published_at IS NULL)),
    CHECK ((state = 'retired') = (retired_at IS NOT NULL))
);
CREATE UNIQUE INDEX billing_plan_version_current ON billing_plan_version(family) WHERE state = 'published';

CREATE TABLE billing_signup_promotion_version (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    promotion text NOT NULL CHECK (promotion IN ('trial', 'card_bonus')),
    version integer NOT NULL CHECK (version > 0),
    state text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'published', 'retired')),
    amount_cents bigint NOT NULL CHECK (amount_cents BETWEEN 1 AND 100000),
    created_by uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_by uuid,
    published_at timestamptz,
    retired_by uuid,
    retired_at timestamptz,
    UNIQUE (promotion, version),
    CHECK ((state = 'draft') = (published_at IS NULL)),
    CHECK ((state = 'retired') = (retired_at IS NOT NULL))
);
CREATE UNIQUE INDEX billing_signup_promotion_current ON billing_signup_promotion_version(promotion) WHERE state = 'published';

CREATE FUNCTION guard_billing_catalog_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.state <> 'draft' THEN RAISE EXCEPTION 'published catalog versions are immutable'; END IF;
        RETURN OLD;
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'draft' THEN RAISE EXCEPTION 'catalog versions start as drafts'; END IF;
        RETURN NEW;
    END IF;
    IF OLD.state = 'draft' THEN
        IF NEW.state = 'retired' THEN RAISE EXCEPTION 'only published versions can be retired'; END IF;
        NEW.updated_at := clock_timestamp();
        RETURN NEW;
    END IF;
    IF OLD.state = 'published' AND NEW.state = 'retired'
        AND (to_jsonb(NEW) - ARRAY['updated_at', 'state', 'retired_by', 'retired_at'])
            = (to_jsonb(OLD) - ARRAY['updated_at', 'state', 'retired_by', 'retired_at']) THEN
        NEW.updated_at := clock_timestamp();
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'published catalog versions are immutable';
END $$;
CREATE TRIGGER billing_plan_version_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_plan_version
    FOR EACH ROW EXECUTE FUNCTION guard_billing_catalog_version();
CREATE TRIGGER billing_signup_promotion_version_guard BEFORE INSERT OR UPDATE OR DELETE ON billing_signup_promotion_version
    FOR EACH ROW EXECUTE FUNCTION guard_billing_catalog_version();
CREATE FUNCTION guard_billing_catalog_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'catalog version history cannot be truncated';
END $$;
CREATE TRIGGER billing_plan_version_truncate_guard BEFORE TRUNCATE ON billing_plan_version
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_truncate();
CREATE TRIGGER billing_signup_promotion_version_truncate_guard BEFORE TRUNCATE ON billing_signup_promotion_version
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_truncate();

-- Signup grants read the published amount, so a promotion is only retired by
-- publishing its replacement in the same transaction.
CREATE FUNCTION require_published_signup_promotion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM billing_signup_promotion_version WHERE promotion = OLD.promotion AND state = 'published') THEN
        RAISE EXCEPTION 'signup promotion % needs a published amount', OLD.promotion;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER billing_signup_promotion_published AFTER UPDATE OF state ON billing_signup_promotion_version
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.state = 'published')
    EXECUTE FUNCTION require_published_signup_promotion();

-- A schedule subscription contracts can represent: one indefinite monthly
-- grant from the anchor, expiring after a month. Returns its amount, or NULL.
CREATE FUNCTION billing_plan_version_monthly_credit(p billing_plan_version) RETURNS bigint
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN jsonb_array_length(p.credit_schedule) = 1
        AND (p.credit_schedule->0->>'start_months')::int = 0
        AND jsonb_typeof(COALESCE(p.credit_schedule->0->'repeat', 'null'::jsonb)) = 'null'
        AND (p.credit_schedule->0->>'every_months')::int = 1
        AND (p.credit_schedule->0->>'expires_after_months')::int = 1
        THEN (p.credit_schedule->0->>'amount_cents')::bigint END;
$$;

-- Keeps the offer rows that contract guards, plan limits and older binaries
-- read in step with the family's current version.
CREATE FUNCTION sync_billing_plan_offers(p_family text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE v billing_plan_version; i integer; fee bigint; credit bigint; k text;
BEGIN
    SELECT * INTO v FROM billing_plan_version WHERE family = p_family AND state = 'published';
    IF NOT FOUND OR v.display_only THEN
        UPDATE billing_plan SET available = false WHERE family = p_family;
        RETURN;
    END IF;
    credit := billing_plan_version_monthly_credit(v);
    FOREACH i IN ARRAY ARRAY[1, 12] LOOP
        k := p_family || CASE i WHEN 1 THEN '_monthly' ELSE '_annual' END;
        fee := CASE i WHEN 1 THEN v.monthly_fee_cents ELSE v.annual_fee_cents END;
        IF fee IS NULL THEN
            UPDATE billing_plan SET available = false WHERE offer_key = k;
            CONTINUE;
        END IF;
        INSERT INTO plan_limits(offer_key, max_sandboxes, max_template_vcpu, max_template_memory_mib, max_template_disk_mib)
        VALUES (k, v.max_sandboxes, v.max_template_vcpu, v.max_template_memory_mib, v.max_template_disk_mib)
        ON CONFLICT (offer_key) DO UPDATE SET max_sandboxes = EXCLUDED.max_sandboxes, max_template_vcpu = EXCLUDED.max_template_vcpu,
            max_template_memory_mib = EXCLUDED.max_template_memory_mib, max_template_disk_mib = EXCLUDED.max_template_disk_mib;
        INSERT INTO billing_plan(offer_key, family, display_name, charge_interval_months, charge_amount_cents, monthly_credit_cents, available, sort_order)
        VALUES (k, p_family, v.display_name, i, fee, COALESCE(credit, 1), v.self_serve AND v.public AND credit IS NOT NULL AND v.matching_bps IS NULL,
            v.sort_order * 10 + CASE i WHEN 1 THEN 0 ELSE 1 END)
        ON CONFLICT (offer_key) DO UPDATE SET display_name = EXCLUDED.display_name, charge_amount_cents = EXCLUDED.charge_amount_cents,
            monthly_credit_cents = CASE WHEN credit IS NULL THEN billing_plan.monthly_credit_cents ELSE EXCLUDED.monthly_credit_cents END,
            available = EXCLUDED.available, sort_order = EXCLUDED.sort_order;
    END LOOP;
END $$;

-- Both take the family lock before any row lock, so a concurrent publish and
-- retire of one family serialize instead of deadlocking.
CREATE FUNCTION publish_billing_catalog_version(p_table text, p_id uuid, p_actor uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE c text; k text; s text;
BEGIN
    IF p_table NOT IN ('billing_plan_version', 'billing_signup_promotion_version') THEN
        RAISE EXCEPTION 'unknown catalog %', p_table;
    END IF;
    c := CASE p_table WHEN 'billing_plan_version' THEN 'family' ELSE 'promotion' END;
    EXECUTE format('SELECT %I FROM %I WHERE id = $1', c, p_table) INTO k USING p_id;
    IF k IS NULL THEN RAISE EXCEPTION 'catalog version not found' USING ERRCODE = 'no_data_found'; END IF;
    PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:' || p_table || ':' || k)::bigint);
    EXECUTE format('SELECT state FROM %I WHERE id = $1 FOR UPDATE', p_table) INTO s USING p_id;
    IF s IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'only drafts can be published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    -- A retried or stale publish of an older version must never replace a
    -- newer one that was published since.
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I n, %I d WHERE d.id = $1 AND n.%I = $2 AND n.state = %L AND n.version > d.version)',
        p_table, p_table, c, 'published') INTO STRICT s USING p_id, k;
    IF s THEN
        RAISE EXCEPTION 'a newer version is already published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    EXECUTE format('UPDATE %I SET state = %L, retired_by = $2, retired_at = clock_timestamp() WHERE %I = $1 AND state = %L',
        p_table, 'retired', c, 'published') USING k, p_actor;
    EXECUTE format('UPDATE %I SET state = %L, published_by = $2, published_at = clock_timestamp() WHERE id = $1',
        p_table, 'published') USING p_id, p_actor;
END $$;

CREATE FUNCTION retire_billing_plan_version(p_id uuid, p_actor uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE f text; s text;
BEGIN
    SELECT family INTO f FROM billing_plan_version WHERE id = p_id;
    IF f IS NULL THEN RAISE EXCEPTION 'catalog version not found' USING ERRCODE = 'no_data_found'; END IF;
    PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:billing_plan_version:' || f)::bigint);
    SELECT state INTO s FROM billing_plan_version WHERE id = p_id FOR UPDATE;
    IF s IS DISTINCT FROM 'published' THEN
        RAISE EXCEPTION 'only the published version can be retired' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE billing_plan_version SET state = 'retired', retired_by = p_actor, retired_at = clock_timestamp() WHERE id = p_id;
END $$;

-- Every state change, however it is made, resyncs the family's offer rows.
CREATE FUNCTION sync_billing_plan_version_offers() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM sync_billing_plan_offers(NEW.family);
    RETURN NULL;
END $$;
CREATE TRIGGER billing_plan_version_sync AFTER UPDATE OF state ON billing_plan_version
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state) EXECUTE FUNCTION sync_billing_plan_version_offers();

-- Grant-time amount of a signup promotion. Promotions cannot be retired
-- without a replacement, so a published version always exists.
CREATE FUNCTION billing_signup_promotion_cents(p_promotion text) RETURNS bigint
LANGUAGE plpgsql STABLE AS $$
DECLARE v bigint;
BEGIN
    SELECT amount_cents INTO v FROM billing_signup_promotion_version WHERE promotion = p_promotion AND state = 'published';
    IF v IS NULL THEN RAISE EXCEPTION 'signup promotion % has no published amount', p_promotion; END IF;
    RETURN v;
END $$;

DO $$ DECLARE r text; t text; BEGIN
    FOREACH t IN ARRAY ARRAY['billing_plan_version', 'billing_signup_promotion_version'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON %I FROM %I', t, r);
                IF r = 'service_role' THEN
                    EXECUTE format('GRANT SELECT ON %I TO service_role', t);
                    EXECUTE format('CREATE POLICY %I ON %I TO service_role USING (true)', t || '_backend', t);
                END IF;
            END IF;
        END LOOP;
    END LOOP;
END $$;
DO $$ DECLARE r text; f text; BEGIN
    FOREACH f IN ARRAY ARRAY['billing_credit_schedule_valid(jsonb)', 'guard_billing_catalog_version()', 'guard_billing_catalog_truncate()',
        'require_published_signup_promotion()', 'billing_plan_version_monthly_credit(billing_plan_version)', 'sync_billing_plan_offers(text)',
        'publish_billing_catalog_version(text,uuid,uuid)', 'retire_billing_plan_version(uuid,uuid)', 'sync_billing_plan_version_offers()',
        'billing_signup_promotion_cents(text)'] LOOP
        EXECUTE 'REVOKE ALL ON FUNCTION ' || f || ' FROM PUBLIC';
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I', f, r);
            END IF;
        END LOOP;
    END LOOP;
END $$;

-- Version 1 of every family reproduces the terms the catalog sold before.
-- Seeded ids are derived from the family so every region holds the same
-- version under the same id.
INSERT INTO billing_plan_version(id, family, version, public, display_name, tagline, features, sort_order, self_serve,
    monthly_fee_cents, annual_fee_cents, credit_schedule, max_sandboxes, max_template_vcpu, max_template_memory_mib, max_template_disk_mib)
SELECT md5('billing-plan-version:' || m.family || ':1')::uuid, m.family, 1, true, m.display_name,
    CASE m.family WHEN 'pro' THEN 'For teams running agents in production at scale.' ELSE '' END,
    CASE m.family WHEN 'pro' THEN ARRAY['Priority support'] ELSE '{}'::text[] END,
    m.sort_order / 10, m.available AND a.available,
    m.charge_amount_cents, a.charge_amount_cents,
    jsonb_build_array(jsonb_build_object('start_months', 0, 'repeat', NULL, 'every_months', 1, 'amount_cents', m.monthly_credit_cents, 'expires_after_months', 1)),
    l.max_sandboxes, l.max_template_vcpu, l.max_template_memory_mib, l.max_template_disk_mib
FROM billing_plan m JOIN billing_plan a ON a.family = m.family AND a.charge_interval_months = 12
JOIN plan_limits l ON l.offer_key = m.offer_key
WHERE m.charge_interval_months = 1 AND m.monthly_credit_cents = a.monthly_credit_cents;
INSERT INTO billing_plan_version(id, family, version, public, display_name, tagline, features, sort_order, display_only)
VALUES (md5('billing-plan-version:enterprise:1')::uuid, 'enterprise', 1, true, 'Enterprise', 'For teams that need self-hosting and dedicated support.',
    ARRAY['Self-host in your AWS, GCP, or Azure account', 'Custom concurrency and sandbox sizes', 'Dedicated support'], 3, true);

-- The amounts that were hardcoded. Binaries released before this migration
-- send and verify a $95 card bonus, so a different amount is published from
-- the catalog only once they have drained.
INSERT INTO billing_signup_promotion_version(id, promotion, version, amount_cents) VALUES
    (md5('billing-signup-promotion:trial:1')::uuid, 'trial', 1, 500),
    (md5('billing-signup-promotion:card_bonus:1')::uuid, 'card_bonus', 1, 9500);
SELECT publish_billing_catalog_version('billing_plan_version', id, NULL) FROM billing_plan_version;
SELECT publish_billing_catalog_version('billing_signup_promotion_version', id, NULL) FROM billing_signup_promotion_version;

-- The card bonus amount is frozen when the Stripe grant is first attempted, so
-- every retry sends the same amount under the same idempotency key. NULL is
-- the historical $95 grant.
ALTER TABLE team_billing_account ADD COLUMN stripe_activation_credit_amount_cents bigint
    CHECK (stripe_activation_credit_amount_cents > 0);

-- Unchanged apart from recording the frozen card bonus instead of $95.
CREATE OR REPLACE FUNCTION finalize_stripe_promotion(p_team_id uuid, p_user_id uuid, p_stripe_grant_id text)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE v_account team_billing_account; v_history_key text;
BEGIN
    IF NULLIF(BTRIM(p_stripe_grant_id), '') IS NULL THEN
        RETURN;
    END IF;
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'Stripe promotion finalization requires a pinned actor reservation'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    PERFORM lock_stripe_promotion(p_team_id, p_user_id);
    SELECT * INTO v_account FROM team_billing_account WHERE team_id = p_team_id FOR UPDATE;
    IF NOT FOUND THEN RETURN; END IF;
    IF v_account.stripe_activation_credit_grant_id IS NOT NULL
       OR v_account.stripe_activation_credit_granted_at IS NOT NULL THEN RETURN; END IF;
    IF v_account.stripe_activation_user_id IS DISTINCT FROM p_user_id
       OR v_account.stripe_activation_identity_key IS NULL
       OR NOT EXISTS (
           SELECT 1 FROM promotion_identity i
           WHERE i.identity_key = v_account.stripe_activation_identity_key
             AND i.stripe_reserved_team_id = p_team_id AND i.stripe_reserved_user_id = p_user_id
       ) THEN
        RAISE EXCEPTION 'Stripe promotion finalization requires its pinned reservation'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE promotion_identity
    SET stripe_redemption_at = COALESCE(stripe_redemption_at, now()),
        stripe_reserved_team_id = NULL, stripe_reserved_user_id = NULL
    WHERE identity_key = v_account.stripe_activation_identity_key;
    UPDATE user_promotion_entitlement
    SET stripe_redemption_at = COALESCE(stripe_redemption_at, now()),
        stripe_redemption_team_id = p_team_id,
        stripe_redemption_reserved_team_id = NULL, stripe_redemption_reserved_at = NULL,
        stripe_redemption_attempted_at = NULL, updated_at = now()
    WHERE user_id = p_user_id AND stripe_redemption_reserved_team_id = p_team_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'Stripe promotion user reservation is missing'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE team_billing_account
    SET stripe_activation_credit_granted_at = now(), stripe_activation_credit_grant_id = p_stripe_grant_id,
        stripe_activation_credit_reserved_at = NULL, updated_at = now()
    WHERE team_id = p_team_id;
    IF v_account.stripe_activation_identity_key LIKE 'legacy:%' THEN
        v_history_key := 'stripe-transition:' || p_team_id::text || ':' || COALESCE(v_account.stripe_activation_credit_reservation_event_id, '');
        IF NOT EXISTS (SELECT 1 FROM promotion_identity_history WHERE history_key = v_history_key) THEN
            v_history_key := 'stripe-user:' || p_user_id::text;
        END IF;
        PERFORM record_promotion_identity_grant(v_history_key, 'stripe', p_user_id, p_team_id, now(), v_account.stripe_activation_identity_evidence_version);
    END IF;
    PERFORM record_stripe_promotion_device_grant(p_team_id, p_user_id);
    IF NOT EXISTS (SELECT 1 FROM team_credit_grant WHERE team_id = p_team_id AND reason = 'signup trial credit') THEN
        INSERT INTO team_credit_grant(team_id, amount_usd, remaining_usd, reason, created_by)
        VALUES (p_team_id, COALESCE(v_account.stripe_activation_credit_amount_cents, 9500) / 100.0, 0, 'stripe promotional credit', p_user_id);
    END IF;
END;
$$;

-- Unchanged apart from granting the configured trial amount instead of $5.
CREATE OR REPLACE FUNCTION claim_team_signup_trial_without_device(p_team_id uuid, p_user_id uuid)
RETURNS TABLE(outcome text, reason text) LANGUAGE plpgsql AS $$
DECLARE p team_signup_trial_provenance; v_key text; v_claimed boolean; v_outcome text; v_reason text;
    v_enabled boolean; v_version uuid; v_trial numeric;
BEGIN
    IF p_user_id IS NULL THEN RAISE EXCEPTION 'signup trial requires a creator' USING ERRCODE = '22023'; END IF;
    v_enabled := canonical_promotion_identity_enabled();
    PERFORM 1 FROM team WHERE id = p_team_id FOR NO KEY UPDATE NOWAIT;
    IF NOT FOUND THEN RAISE EXCEPTION 'signup team does not exist' USING ERRCODE = '22023'; END IF;
    IF EXISTS (SELECT 1 FROM team_signup_promotion_outcome o WHERE o.team_id = p_team_id AND o.user_id <> p_user_id) THEN
        RAISE EXCEPTION 'signup trial creator cannot change' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY SELECT o.outcome, o.reason FROM team_signup_promotion_outcome o WHERE o.team_id = p_team_id;
    IF FOUND THEN RETURN; END IF;
    SELECT * INTO p FROM team_signup_trial_provenance WHERE team_id = p_team_id FOR UPDATE;
    IF NOT FOUND OR p.completed_at IS NOT NULL THEN
        RETURN QUERY SELECT 'promotion_ineligible'::text, 'not_initial_creation'::text;
        RETURN;
    END IF;
    IF p.creator_bound_at IS NOT NULL AND p.creator_user_id IS DISTINCT FROM p_user_id THEN
        RAISE EXCEPTION 'signup trial creator cannot change' USING ERRCODE = '22023';
    END IF;
    v_version := capture_promotion_identity_evidence(p_user_id);
    IF v_version IS NOT NULL THEN v_key := resolve_promotion_identity_evidence(p_user_id, v_version); END IF;
    v_claimed := false;
    IF v_enabled AND v_key IS NULL THEN
        v_outcome := 'promotion_ineligible'; v_reason := 'verified_identity_missing';
    ELSIF v_enabled AND EXISTS (SELECT 1 FROM promotion_identity i WHERE i.identity_key = v_key AND i.signup_claimed_at IS NOT NULL) THEN
        v_outcome := 'already_claimed'; v_reason := 'identity_already_claimed';
    ELSIF v_enabled AND promotion_identity_history_pending('signup') THEN
        v_outcome := 'promotion_ineligible'; v_reason := 'historical_identity_unresolved';
    ELSIF EXISTS (SELECT 1 FROM promotion_identity_history h
        WHERE h.promotion = 'signup' AND h.user_id = p_user_id AND h.grant_state = 'granted'
          AND h.team_id IS DISTINCT FROM p_team_id) THEN
        v_outcome := 'already_claimed'; v_reason := 'user_already_claimed';
    ELSE
        INSERT INTO user_signup_trial_claim(user_id, team_id) VALUES(p_user_id, p_team_id)
        ON CONFLICT (user_id) DO NOTHING RETURNING true INTO v_claimed;
        IF v_claimed OR EXISTS (SELECT 1 FROM user_signup_trial_claim WHERE user_id = p_user_id AND team_id = p_team_id) THEN
            INSERT INTO user_promotion_entitlement(user_id, signup_trial_claimed_at, signup_trial_team_id)
            VALUES(p_user_id, now(), p_team_id)
            ON CONFLICT (user_id) DO UPDATE SET
                signup_trial_claimed_at = COALESCE(user_promotion_entitlement.signup_trial_claimed_at, EXCLUDED.signup_trial_claimed_at),
                signup_trial_team_id = CASE WHEN user_promotion_entitlement.signup_trial_claimed_at IS NULL
                    THEN EXCLUDED.signup_trial_team_id ELSE user_promotion_entitlement.signup_trial_team_id END;
            PERFORM record_promotion_identity_grant('signup-grant:' || p_team_id::text,
                'signup', p_user_id, p_team_id, now(), CASE WHEN p.legacy_grant_id IS NULL THEN v_version END);
            IF p.legacy_grant_id IS NULL THEN
                v_trial := billing_signup_promotion_cents('trial') / 100.0;
                INSERT INTO team_credit_grant(team_id, amount_usd, remaining_usd, reason, created_by)
                VALUES(p_team_id, v_trial, v_trial, 'signup trial credit', p_user_id);
            ELSE
                UPDATE team_credit_grant g SET created_by = p_user_id
                WHERE g.id = p.legacy_grant_id AND g.team_id = p_team_id AND g.reason = 'signup trial credit' AND g.created_by IS NULL;
            END IF;
            INSERT INTO team_trial_eligibility_cache(team_id, eligible) VALUES(p_team_id, true)
            ON CONFLICT (team_id) DO UPDATE SET eligible = true, updated_at = now();
            v_outcome := 'granted'; v_reason := CASE WHEN v_enabled THEN 'first_identity_claim' ELSE 'first_user_claim' END;
        ELSE
            v_outcome := 'already_claimed'; v_reason := 'user_already_claimed';
        END IF;
    END IF;
    IF v_outcome <> 'granted' AND p.legacy_grant_id IS NULL THEN
        INSERT INTO team_signup_trial_denial(team_id) VALUES(p_team_id) ON CONFLICT DO NOTHING;
    END IF;
    INSERT INTO team_signup_promotion_outcome(team_id, user_id, identity_key, outcome, reason)
    VALUES(p_team_id, p_user_id, v_key, v_outcome, v_reason);
    UPDATE team_signup_trial_provenance SET creator_user_id = p_user_id,
        creator_bound_at = COALESCE(creator_bound_at, now()), completed_at = now() WHERE team_id = p_team_id;
    RETURN QUERY SELECT v_outcome, v_reason;
END $$;
REVOKE ALL ON FUNCTION claim_team_signup_trial_without_device(uuid,uuid) FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON FUNCTION claim_team_signup_trial_without_device(uuid,uuid) FROM %I', r);
        END IF;
    END LOOP;
END $$;

-- Every staff change to the catalog, with who made it and why. Rows are
-- append-only; the terms column snapshots the version as the change left it.
CREATE TABLE billing_catalog_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('create_draft', 'update_draft', 'delete_draft', 'publish', 'retire', 'publish_promotion')),
    version_id uuid NOT NULL,
    reason text NOT NULL CHECK (btrim(reason) <> '' AND length(reason) <= 500),
    terms jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX billing_catalog_audit_version ON billing_catalog_audit(version_id, created_at);
CREATE FUNCTION guard_billing_catalog_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'billing catalog audit is append-only';
END $$;
CREATE TRIGGER billing_catalog_audit_guard BEFORE UPDATE OR DELETE ON billing_catalog_audit
    FOR EACH ROW EXECUTE FUNCTION guard_billing_catalog_audit();
CREATE TRIGGER billing_catalog_audit_truncate_guard BEFORE TRUNCATE ON billing_catalog_audit
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_catalog_audit();
ALTER TABLE billing_catalog_audit ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_catalog_audit FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON billing_catalog_audit FROM %I', r);
            IF r = 'service_role' THEN
                GRANT SELECT ON billing_catalog_audit TO service_role;
                CREATE POLICY billing_catalog_audit_backend ON billing_catalog_audit TO service_role USING (true);
            END IF;
        END IF;
    END LOOP;
END $$;
REVOKE ALL ON FUNCTION guard_billing_catalog_audit() FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON FUNCTION guard_billing_catalog_audit() FROM %I', r);
        END IF;
    END LOOP;
END $$;

-- Limits a contract was sold with. Contracts with a row keep these limits
-- whatever later catalog versions say; older contracts fall back to the
-- offer's current plan_limits. Manually customised teams keep their limits.
CREATE TABLE billing_contract_limits (
    contract_id uuid PRIMARY KEY REFERENCES billing_commercial_contract(id),
    max_sandboxes integer NOT NULL CHECK (max_sandboxes > 0),
    max_template_vcpu integer CHECK (max_template_vcpu > 0),
    max_template_memory_mib integer CHECK (max_template_memory_mib > 0),
    max_template_disk_mib integer CHECK (max_template_disk_mib > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION guard_billing_contract_limits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'contract limits are frozen';
END $$;
CREATE TRIGGER billing_contract_limits_guard BEFORE UPDATE OR DELETE ON billing_contract_limits
    FOR EACH ROW EXECUTE FUNCTION guard_billing_contract_limits();
CREATE TRIGGER billing_contract_limits_truncate_guard BEFORE TRUNCATE ON billing_contract_limits
    FOR EACH STATEMENT EXECUTE FUNCTION guard_billing_contract_limits();

CREATE FUNCTION apply_contract_limits(p_contract uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE c billing_commercial_contract; l billing_contract_limits;
BEGIN
    SELECT * INTO c FROM billing_commercial_contract WHERE id = p_contract;
    IF NOT FOUND OR c.status <> 'active' OR c.charge_kind = 'sponsored_entitlement' THEN RETURN; END IF;
    SELECT * INTO l FROM billing_contract_limits WHERE contract_id = p_contract;
    IF NOT FOUND THEN
        PERFORM apply_plan_limits(c.team_id, c.offer_key);
        RETURN;
    END IF;
    PERFORM set_config('superserve.applying_plan_limits', 'on', true);
    UPDATE team SET max_sandboxes = l.max_sandboxes, max_template_vcpu = l.max_template_vcpu,
        max_template_memory_mib = l.max_template_memory_mib, max_template_disk_mib = l.max_template_disk_mib
    WHERE id = c.team_id AND NOT limits_customized;
    PERFORM set_config('superserve.applying_plan_limits', 'off', true);
END $$;

-- Unchanged apart from preferring the contract's frozen limits.
CREATE OR REPLACE FUNCTION billing_contract_apply_plan_limits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.charge_kind<>'sponsored_entitlement' THEN
  IF NEW.status='active' THEN
   PERFORM apply_contract_limits(NEW.id);
  ELSE
   PERFORM apply_plan_limits(NEW.team_id,'free');
  END IF;
 END IF;
 RETURN NULL;
END $$;

-- Limits frozen after the contract activated take effect immediately.
CREATE FUNCTION billing_contract_limits_apply() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM apply_contract_limits(NEW.contract_id);
    RETURN NULL;
END $$;
CREATE TRIGGER billing_contract_limits_apply AFTER INSERT ON billing_contract_limits
    FOR EACH ROW EXECUTE FUNCTION billing_contract_limits_apply();

ALTER TABLE billing_contract_limits ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON billing_contract_limits FROM PUBLIC;
DO $$ DECLARE r text; f text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON billing_contract_limits FROM %I', r);
            IF r = 'service_role' THEN
                GRANT SELECT ON billing_contract_limits TO service_role;
                CREATE POLICY billing_contract_limits_backend ON billing_contract_limits TO service_role USING (true);
            END IF;
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY['guard_billing_contract_limits()', 'apply_contract_limits(uuid)', 'billing_contract_limits_apply()'] LOOP
        EXECUTE 'REVOKE ALL ON FUNCTION ' || f || ' FROM PUBLIC';
        FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
                EXECUTE format('REVOKE ALL ON FUNCTION %s FROM %I', f, r);
            END IF;
        END LOOP;
    END LOOP;
END $$;

-- Triggers and functions run as the writing role, which can be service_role,
-- so the helpers they call stay executable for it.
DO $$ DECLARE f text; BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
        FOREACH f IN ARRAY ARRAY['billing_credit_schedule_valid(jsonb)', 'billing_plan_version_monthly_credit(billing_plan_version)',
            'sync_billing_plan_offers(text)', 'billing_signup_promotion_cents(text)', 'apply_contract_limits(uuid)'] LOOP
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO service_role', f);
        END LOOP;
    END IF;
END $$;
