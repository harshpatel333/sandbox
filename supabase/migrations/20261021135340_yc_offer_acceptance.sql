CREATE TABLE public.yc_offer_acceptance (
 id uuid PRIMARY KEY,
 claim_id uuid NOT NULL UNIQUE,
 team_id uuid NOT NULL UNIQUE REFERENCES public.team(id),
 user_id uuid NOT NULL,
 home_region text NOT NULL,
 authority jsonb NOT NULL CHECK(jsonb_typeof(authority)='object'),
 terms jsonb NOT NULL CHECK(jsonb_typeof(terms)='object'),
 renewal_terms jsonb NOT NULL CHECK(jsonb_typeof(renewal_terms)='object'),
 terms_digest text NOT NULL CHECK(terms_digest ~ '^[a-f0-9]{64}$'),
 renewal_terms_digest text NOT NULL CHECK(renewal_terms_digest ~ '^[a-f0-9]{64}$'),
 pricing_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 request_id uuid UNIQUE,
 accepted_at timestamptz,
 assignment_id uuid UNIQUE,
 operation_id uuid UNIQUE,
 state text NOT NULL DEFAULT 'offered' CHECK(state IN ('offered','confirming','accepted','assignment_linked','dispatched','held','revoked')),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0,
 lease_id uuid,
 lease_until timestamptz,
 last_error text,
 CHECK ((request_id IS NULL)=(accepted_at IS NULL)),
 CHECK (state='offered' OR accepted_at IS NOT NULL),
 CHECK (operation_id IS NULL OR assignment_id IS NOT NULL)
);
CREATE INDEX yc_offer_acceptance_pending ON public.yc_offer_acceptance(home_region,next_attempt_at)
 WHERE state IN ('confirming','accepted','assignment_linked');
CREATE TABLE public.yc_offer_acceptance_control (
 request_id uuid PRIMARY KEY,
 acceptance_id uuid NOT NULL REFERENCES public.yc_offer_acceptance(id),
 actor_id uuid NOT NULL,
 action text NOT NULL CHECK(action IN ('hold','retry')),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION public.yc_offer_acceptance_preserve() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
 IF TG_OP='DELETE' OR (NEW.id,NEW.claim_id,NEW.team_id,NEW.user_id,NEW.home_region,NEW.authority,NEW.terms,NEW.renewal_terms,NEW.terms_digest,NEW.renewal_terms_digest,NEW.pricing_at,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.claim_id,OLD.team_id,OLD.user_id,OLD.home_region,OLD.authority,OLD.terms,OLD.renewal_terms,OLD.terms_digest,OLD.renewal_terms_digest,OLD.pricing_at,OLD.created_at)
 OR (OLD.accepted_at IS NOT NULL AND (NEW.accepted_at,NEW.request_id) IS DISTINCT FROM (OLD.accepted_at,OLD.request_id))
 OR (OLD.assignment_id IS NOT NULL AND NEW.assignment_id IS DISTINCT FROM OLD.assignment_id)
 OR (OLD.operation_id IS NOT NULL AND NEW.operation_id IS DISTINCT FROM OLD.operation_id)
 OR (OLD.accepted_at IS NOT NULL AND NEW.state='offered') THEN
  RAISE EXCEPTION 'acceptance identity is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER yc_offer_acceptance_preserve BEFORE UPDATE OR DELETE ON public.yc_offer_acceptance
 FOR EACH ROW EXECUTE FUNCTION public.yc_offer_acceptance_preserve();
ALTER TABLE public.yc_offer_acceptance ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.yc_offer_acceptance_control ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON public.yc_offer_acceptance,public.yc_offer_acceptance_control FROM PUBLIC;
REVOKE ALL ON FUNCTION public.yc_offer_acceptance_preserve() FROM PUBLIC;
DO $$ DECLARE role_name text; BEGIN
 FOREACH role_name IN ARRAY ARRAY['anon','authenticated'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('REVOKE ALL ON public.yc_offer_acceptance,public.yc_offer_acceptance_control FROM %I',role_name);
   EXECUTE format('REVOKE ALL ON FUNCTION public.yc_offer_acceptance_preserve() FROM %I',role_name);
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN
  REVOKE ALL ON public.yc_offer_acceptance,public.yc_offer_acceptance_control FROM service_role;
  REVOKE ALL ON FUNCTION public.yc_offer_acceptance_preserve() FROM service_role;
  GRANT SELECT,INSERT,UPDATE ON public.yc_offer_acceptance TO service_role;
  CREATE POLICY service_access ON public.yc_offer_acceptance TO service_role USING(true) WITH CHECK(true);
  GRANT SELECT,INSERT ON public.yc_offer_acceptance_control TO service_role;
  CREATE POLICY service_access ON public.yc_offer_acceptance_control TO service_role USING(true) WITH CHECK(true);
 END IF;
END $$;
