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

CREATE OR REPLACE FUNCTION publish_billing_plan_version(p_id uuid, p_actor uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE d billing_plan_version;
BEGIN
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'catalog version not found' USING ERRCODE = 'no_data_found'; END IF;
    PERFORM 1 FROM billing_pricing_switch FOR SHARE;
    PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:' || d.family || ':' || d.pricing_version)::bigint);
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id FOR UPDATE;
    IF d.state IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'only drafts can be published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF EXISTS (SELECT 1 FROM billing_plan_version n WHERE n.family = d.family AND n.pricing_version = d.pricing_version
        AND n.state IN ('published', 'retired') AND n.version > d.version) THEN
        RAISE EXCEPTION 'a newer version has already been published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE billing_plan_version SET state = 'retired', retired_by = p_actor, retired_at = clock_timestamp()
    WHERE family = d.family AND pricing_version = d.pricing_version AND state = 'published';
    UPDATE billing_plan_version SET state = 'published', published_by = p_actor, published_at = clock_timestamp() WHERE id = p_id;
END $$;

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
