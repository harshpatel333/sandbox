-- Keep a usable default for signup while the raw v2 switch is active.
-- Zero templates is a valid Free cap; other shape and concurrency caps remain
-- positive. Legacy offer/contract limit constraints are intentionally unchanged.
ALTER TABLE public.billing_plan_version DROP CONSTRAINT billing_plan_version_max_templates_check;
ALTER TABLE public.billing_plan_version ADD CONSTRAINT billing_plan_version_max_templates_check CHECK (max_templates >= 0);

CREATE OR REPLACE FUNCTION publish_billing_plan_version(p_id uuid, p_actor uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE d billing_plan_version;
BEGIN
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'catalog version not found' USING ERRCODE = 'no_data_found'; END IF;
    -- A pricing switch flip resyncs every family; holding the switch row
    -- keeps this sync from interleaving with it.
    PERFORM 1 FROM billing_pricing_switch FOR SHARE;
    PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:' || d.family || ':' || d.pricing_version)::bigint);
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id FOR UPDATE;
    IF d.state IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION 'only drafts can be published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    -- A retried or stale publish of an older version must never replace a
    -- newer one that was published since.
    IF EXISTS (SELECT 1 FROM billing_plan_version n WHERE n.family = d.family AND n.pricing_version = d.pricing_version
        AND n.state = 'published' AND n.version > d.version) THEN
        RAISE EXCEPTION 'a newer version is already published' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    -- New teams always freeze this published version. Validate it before the
    -- predecessor is retired, even when the v2 public entry is still closed.
    IF d.pricing_version = 'v2' AND d.family = 'free' AND (
        d.kind IS DISTINCT FROM 'free' OR d.display_only OR d.self_serve
        OR d.monthly_fee_cents IS NOT NULL OR d.annual_fee_cents IS NOT NULL
        OR d.allowance_period IS DISTINCT FROM 'day_window'
        OR d.allowance_on_exhausted IS DISTINCT FROM 'pause'
        OR NOT (COALESCE(d.allowance_cents > 0, false)
            OR COALESCE(d.allowance_compute_seconds > 0 AND d.allowance_compute_vcpu > 0
                AND d.allowance_compute_memory_gib > 0, false))
        OR COALESCE(d.max_sandboxes, 0) <= 0 OR COALESCE(d.max_template_vcpu, 0) <= 0
        OR COALESCE(d.max_template_memory_mib, 0) < 256 OR COALESCE(d.max_template_disk_mib, 0) < 1024
    ) THEN
        RAISE EXCEPTION 'published usable v2 Free catalog required' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE billing_plan_version SET state = 'retired', retired_by = p_actor, retired_at = clock_timestamp()
    WHERE family = d.family AND pricing_version = d.pricing_version AND state = 'published';
    UPDATE billing_plan_version SET state = 'published', published_by = p_actor, published_at = clock_timestamp() WHERE id = p_id;
END $$;

CREATE OR REPLACE FUNCTION retire_billing_plan_version(p_id uuid, p_actor uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE d billing_plan_version;
BEGIN
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id;
    IF NOT FOUND THEN RAISE EXCEPTION 'catalog version not found' USING ERRCODE = 'no_data_found'; END IF;
    -- A pricing switch flip resyncs every family; holding the switch row
    -- keeps this sync from interleaving with it.
    PERFORM 1 FROM billing_pricing_switch FOR SHARE;
    PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:' || d.family || ':' || d.pricing_version)::bigint);
    SELECT * INTO d FROM billing_plan_version WHERE id = p_id FOR UPDATE;
    IF d.state IS DISTINCT FROM 'published' THEN
        RAISE EXCEPTION 'only the published version can be retired' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    -- Use the raw switch, not the adoption-gated public model. Replacement
    -- publication retires its predecessor atomically through the other function.
    IF d.pricing_version = 'v2' AND d.family = 'free'
        AND (SELECT pricing_version FROM billing_pricing_switch WHERE singleton) = 'v2' THEN
        RAISE EXCEPTION 'active v2 Free must be replaced, not retired' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE billing_plan_version SET state = 'retired', retired_by = p_actor, retired_at = clock_timestamp() WHERE id = p_id;
END $$;
