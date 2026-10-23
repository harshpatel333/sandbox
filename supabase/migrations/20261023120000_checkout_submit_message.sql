-- A Checkout replay must send exactly the parameters of the first request for
-- its idempotency key. Whether a PAYG generation shows the submit message is
-- therefore recorded with the generation itself. Writers that predate this
-- column begin generations without touching it, so a stale value from an
-- earlier generation never matches and those generations replay without the
-- message.
ALTER TABLE team_billing_account ADD COLUMN checkout_submit_message_generation timestamptz;

COMMENT ON COLUMN team_billing_account.checkout_submit_message_generation IS
    'checkout_initializing_at of the PAYG Checkout generation whose session includes the submit message; any other value means no message.';

CREATE FUNCTION begin_stripe_checkout_with_publication_decision(
    p_team_id uuid, p_user_id uuid, p_operation_id uuid, p_home_region text,
    p_request_key text, p_decision text, p_attempt_id uuid, p_submit_message boolean)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE a team_billing_account; d stripe_checkout_publication_decision; v_evidence uuid; v_generation timestamptz;
BEGIN
    SET LOCAL lock_timeout = '5s';
    IF p_team_id IS NULL OR p_user_id IS NULL OR p_operation_id IS NULL OR p_attempt_id IS NULL
       OR p_home_region IS NULL OR p_home_region NOT IN ('use','usw')
       OR p_decision IS NULL OR p_decision NOT IN ('standard','publication_failed')
       OR NULLIF(p_request_key,'') IS NULL OR p_submit_message IS NULL THEN
        RAISE EXCEPTION 'invalid Checkout decision' USING ERRCODE='22023';
    END IF;
    -- Match the reservation user/account lock order, without requiring evidence
    -- or promotion policy authority for an explicitly paid-only generation.
    IF p_decision = 'standard' THEN
        PERFORM lock_stripe_checkout_identity(p_user_id);
    ELSE
        PERFORM pg_advisory_xact_lock(hashtext('stripe-promo-user:' || p_user_id::text)::bigint);
    END IF;
    SELECT * INTO a FROM team_billing_account WHERE team_id=p_team_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'Checkout account missing' USING ERRCODE='23505'; END IF;
    IF EXISTS (SELECT 1 FROM stripe_promotion_migration_fence WHERE team_id=p_team_id) THEN
        RAISE EXCEPTION 'Checkout account migrated' USING ERRCODE='23505';
    END IF;
    IF NULLIF(btrim(a.stripe_subscription_id),'') IS NOT NULL
       AND lower(btrim(a.stripe_subscription_status)) IN ('active','trialing','past_due','unpaid','paused') THEN
        RAISE EXCEPTION 'Checkout subscription already established' USING ERRCODE='23505';
    END IF;
    SELECT * INTO d FROM stripe_checkout_publication_decision WHERE operation_id=p_operation_id;
    IF FOUND THEN
        IF d.team_id IS DISTINCT FROM p_team_id OR d.user_id IS DISTINCT FROM p_user_id
           OR d.home_region IS DISTINCT FROM p_home_region OR d.request_key IS DISTINCT FROM p_request_key
           OR d.decision IS DISTINCT FROM p_decision
           OR a.checkout_initializing_at IS DISTINCT FROM d.checkout_generation
           OR a.stripe_checkout_actor_id IS DISTINCT FROM p_user_id
           OR a.checkout_request_key IS DISTINCT FROM p_request_key
           OR a.checkout_completed_at IS NOT NULL
           OR a.checkout_initializing_at <= now() - interval '23 hours' THEN
            RAISE EXCEPTION 'Checkout intent conflict or closed generation' USING ERRCODE='23505';
        END IF;
        -- Compaction forgets attempt identifiers, never the possibility that
        -- one of those requests reached Stripe. All retries retain the same
        -- generation/idempotency key and its existing replay deadline. The
        -- submit message also stays whatever the generation began with.
        UPDATE team_billing_account SET checkout_pending_attempt_ids =
            CASE
                WHEN p_attempt_id=ANY(checkout_pending_attempt_ids) THEN checkout_pending_attempt_ids
                WHEN cardinality(checkout_pending_attempt_ids) >= 32 THEN ARRAY[p_attempt_id]
                ELSE array_append(checkout_pending_attempt_ids,p_attempt_id)
            END,
            checkout_may_exist = checkout_may_exist OR (
                cardinality(checkout_pending_attempt_ids) >= 32
                AND p_attempt_id <> ALL(checkout_pending_attempt_ids)),
            updated_at=now() WHERE team_id=p_team_id;
        RETURN;
    END IF;
    IF a.checkout_initializing_at IS NOT NULL OR a.checkout_completed_at IS NOT NULL
       OR a.stripe_activation_credit_reserved_at IS NOT NULL THEN
        RAISE EXCEPTION 'Checkout generation already in progress' USING ERRCODE='23505';
    END IF;
    IF p_decision='standard' THEN v_evidence := capture_promotion_identity_evidence(p_user_id); END IF;
    v_generation := clock_timestamp();
    UPDATE team_billing_account SET
        checkout_initializing_at=v_generation, checkout_request_key=p_request_key,
        checkout_submit_message_generation=CASE WHEN p_submit_message THEN v_generation END,
        checkout_pending_attempt_ids=ARRAY[p_attempt_id], checkout_may_exist=false,
        checkout_subscription_id=NULL, checkout_completed_at=NULL, checkout_session_id=NULL,
        stripe_checkout_actor_id=p_user_id, stripe_checkout_actor_claimed_at=now(),
        stripe_checkout_identity_evidence_version=v_evidence,
        checkout_anchor_snapshot=COALESCE(checkout_anchor_snapshot,commercial_billing_anchor), updated_at=now()
    WHERE team_id=p_team_id RETURNING * INTO a;
    INSERT INTO stripe_checkout_publication_decision
        (team_id,checkout_generation,user_id,operation_id,home_region,request_key,decision)
    VALUES(p_team_id,a.checkout_initializing_at,p_user_id,p_operation_id,p_home_region,p_request_key,p_decision);
END $$;

-- Writers that predate the submit message keep calling the original overload.
CREATE OR REPLACE FUNCTION begin_stripe_checkout_with_publication_decision(
    p_team_id uuid, p_user_id uuid, p_operation_id uuid, p_home_region text,
    p_request_key text, p_decision text, p_attempt_id uuid)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
    SELECT begin_stripe_checkout_with_publication_decision(
        p_team_id, p_user_id, p_operation_id, p_home_region, p_request_key, p_decision, p_attempt_id, false)
$$;

REVOKE ALL ON FUNCTION begin_stripe_checkout_with_publication_decision(uuid,uuid,uuid,text,text,text,uuid,boolean) FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname=r) THEN
            EXECUTE format('REVOKE ALL ON FUNCTION begin_stripe_checkout_with_publication_decision(uuid,uuid,uuid,text,text,text,uuid,boolean) FROM %I',r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
        GRANT EXECUTE ON FUNCTION begin_stripe_checkout_with_publication_decision(uuid,uuid,uuid,text,text,text,uuid,boolean) TO service_role;
    END IF;
END $$;
