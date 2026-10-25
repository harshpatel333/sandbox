BEGIN;

SELECT * FROM billing_lock_pricing_public_state();
LOCK TABLE billing_plan_version, plan_limits, billing_contract_limits IN ACCESS EXCLUSIVE MODE NOWAIT;
DO $$ BEGIN
    IF NOT pg_try_advisory_xact_lock(hashtext('billing-catalog:free:v2')::bigint) THEN
        RAISE EXCEPTION 'Free catalog is busy; retry the migration' USING ERRCODE = 'lock_not_available';
    END IF;
END $$;

ALTER TABLE billing_plan_version
    DROP CONSTRAINT billing_plan_version_max_templates_check,
    ADD CONSTRAINT billing_plan_version_max_templates_check CHECK (max_templates >= 0);
ALTER TABLE plan_limits
    DROP CONSTRAINT plan_limits_max_templates_check,
    ADD CONSTRAINT plan_limits_max_templates_check CHECK (max_templates >= 0);
ALTER TABLE billing_contract_limits
    DROP CONSTRAINT billing_contract_limits_max_templates_check,
    ADD CONSTRAINT billing_contract_limits_max_templates_check CHECK (max_templates >= 0);

DO $$
DECLARE
    predecessor billing_plan_version;
    successor billing_plan_version;
BEGIN
    SELECT * INTO STRICT predecessor FROM billing_plan_version
    WHERE pricing_version = 'v2' AND family = 'free' AND kind = 'free' AND state = 'published'
    FOR UPDATE;
    IF predecessor.max_templates = 0 THEN
        RETURN;
    END IF;

    successor := predecessor;
    successor.id := md5('billing-plan-version:v2:free:no-custom-templates:1')::uuid;
    SELECT max(version) + 1 INTO successor.version FROM billing_plan_version WHERE family = 'free';
    successor.state := 'draft';
    successor.max_templates := 0;
    successor.created_by := NULL;
    successor.created_at := clock_timestamp();
    successor.updated_at := successor.created_at;
    successor.published_by := NULL;
    successor.published_at := NULL;
    successor.retired_by := NULL;
    successor.retired_at := NULL;

    INSERT INTO billing_plan_version SELECT successor.*;
    PERFORM publish_billing_plan_version(successor.id, NULL);
END $$;

COMMIT;
