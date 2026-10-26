-- The first staff decision is authoritative across console processes and regions.
-- This table must also be installed on the primary Auth project used by console.
CREATE TABLE public.signup_review_decision (
    user_id uuid PRIMARY KEY,
    decision text NOT NULL CHECK (decision IN ('approved', 'rejected')),
    staff_id uuid NOT NULL,
    plan_version_id uuid,
    region text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp() CHECK (isfinite(created_at)),
    CHECK ((decision = 'approved' AND plan_version_id IS NOT NULL AND region IS NOT NULL AND btrim(region) <> '')
        OR (decision = 'rejected' AND plan_version_id IS NULL AND region IS NULL))
);

CREATE FUNCTION public.guard_signup_review_decision() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    RAISE EXCEPTION 'signup review decisions are immutable' USING ERRCODE = '55000';
END;
$$;
CREATE TRIGGER signup_review_decision_immutable
    BEFORE UPDATE OR DELETE ON public.signup_review_decision
    FOR EACH ROW EXECUTE FUNCTION public.guard_signup_review_decision();
CREATE TRIGGER signup_review_decision_truncate_guard
    BEFORE TRUNCATE ON public.signup_review_decision
    FOR EACH STATEMENT EXECUTE FUNCTION public.guard_signup_review_decision();

ALTER TABLE public.signup_review_decision ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON public.signup_review_decision FROM PUBLIC;
REVOKE ALL ON FUNCTION public.guard_signup_review_decision() FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON public.signup_review_decision FROM %I', r);
            EXECUTE format('REVOKE ALL ON FUNCTION public.guard_signup_review_decision() FROM %I', r);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
        GRANT SELECT, INSERT ON public.signup_review_decision TO service_role;
        CREATE POLICY signup_review_decision_backend ON public.signup_review_decision
            TO service_role USING (true) WITH CHECK (true);
    END IF;
END $$;
