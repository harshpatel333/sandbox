-- A requested move of a team to another plan that takes effect later or needs
-- follow-up work: downgrades wait for the end of the billing period and can be
-- undone until then; a move to Free pauses the team's running sandboxes once
-- it applies. Upgrades are not recorded here: they apply on payment.
CREATE TABLE team_plan_change (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 team_id uuid NOT NULL REFERENCES team(id),
 from_assignment_id uuid NOT NULL REFERENCES team_plan_assignment(id),
 target_plan_key text NOT NULL CHECK(btrim(target_plan_key)<>''),
 target_kind text NOT NULL CHECK(target_kind IN ('free','prepaid','subscription','legacy_payg','custom')),
 target_terms jsonb NOT NULL CHECK(jsonb_typeof(target_terms)='object'),
 target_limits jsonb NOT NULL CHECK(jsonb_typeof(target_limits)='object' AND jsonb_typeof(target_limits->'max_sandboxes')='number'),
 effective_at timestamptz NOT NULL,
 pauses_running boolean NOT NULL,
 subscription_id text CHECK(subscription_id IS NULL OR btrim(subscription_id)<>''),
 requested_by uuid NOT NULL,
 requested_via text NOT NULL CHECK(requested_via IN ('customer','staff')),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 2000),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),
 requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 state text NOT NULL DEFAULT 'scheduled' CHECK(state IN ('scheduled','applied','completed','undone')),
 provider_cancel_at timestamptz,
 undone_at timestamptz,
 undone_by uuid,
 applied_at timestamptz,
 applied_assignment_id uuid UNIQUE REFERENCES team_plan_assignment(id),
 completed_at timestamptz,
 next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_token uuid,
 lease_until timestamptz,
 failures integer NOT NULL DEFAULT 0 CHECK(failures>=0),
 last_error text,
 UNIQUE(team_id,idempotency_key),
 CHECK((state='undone')=(undone_at IS NOT NULL) AND (state IN ('applied','completed'))=(applied_at IS NOT NULL AND applied_assignment_id IS NOT NULL)
  AND (state='completed')=(completed_at IS NOT NULL))
);
CREATE UNIQUE INDEX team_plan_change_pending ON team_plan_change(team_id) WHERE state='scheduled';
CREATE INDEX team_plan_change_due ON team_plan_change(next_check_at,id) WHERE state IN ('scheduled','applied');
CREATE INDEX team_plan_change_history ON team_plan_change(team_id,requested_at DESC);

CREATE FUNCTION guard_team_plan_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bookkeeping text[]:=ARRAY['next_check_at','lease_token','lease_until','failures','last_error'];
BEGIN
 IF TG_OP<>'UPDATE' AND TG_OP<>'INSERT' THEN RAISE EXCEPTION 'plan change history is immutable'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'scheduled' OR num_nonnulls(NEW.provider_cancel_at,NEW.undone_at,NEW.undone_by,NEW.applied_at,NEW.applied_assignment_id,NEW.completed_at)<>0 THEN
   RAISE EXCEPTION 'a plan change starts scheduled';
  END IF;
  RETURN NEW;
 END IF;
 IF (to_jsonb(NEW)-bookkeeping-ARRAY['state','effective_at','provider_cancel_at','undone_at','undone_by','applied_at','applied_assignment_id','completed_at'])
  IS DISTINCT FROM (to_jsonb(OLD)-bookkeeping-ARRAY['state','effective_at','provider_cancel_at','undone_at','undone_by','applied_at','applied_assignment_id','completed_at']) THEN
  RAISE EXCEPTION 'a plan change request is immutable';
 END IF;
 IF OLD.state IN ('completed','undone') AND (to_jsonb(NEW)-bookkeeping) IS DISTINCT FROM (to_jsonb(OLD)-bookkeeping) THEN
  RAISE EXCEPTION 'a finished plan change is immutable';
 END IF;
 IF NEW.state IS DISTINCT FROM OLD.state AND NOT (OLD.state,NEW.state) IN (('scheduled','applied'),('scheduled','undone'),('applied','completed')) THEN
  RAISE EXCEPTION 'plan change cannot move from % to %',OLD.state,NEW.state;
 END IF;
 IF OLD.provider_cancel_at IS NOT NULL AND NEW.provider_cancel_at IS DISTINCT FROM OLD.provider_cancel_at
  OR NEW.provider_cancel_at IS NOT NULL AND OLD.provider_cancel_at IS NULL AND OLD.state<>'scheduled' THEN
  RAISE EXCEPTION 'a subscription cancellation is recorded once, while scheduled';
 END IF;
 -- A change whose subscription cancellation never reached Stripe before the
 -- period ended waits for the next period end rather than applying early.
 IF NEW.effective_at IS DISTINCT FROM OLD.effective_at AND (OLD.state<>'scheduled' OR NEW.state<>'scheduled'
  OR OLD.provider_cancel_at IS NOT NULL OR NEW.effective_at<=OLD.effective_at) THEN
  RAISE EXCEPTION 'only an unconfirmed change can move to a later effective time';
 END IF;
 IF NEW.state='applied' AND OLD.state='scheduled' AND NEW.effective_at>clock_timestamp() THEN
  RAISE EXCEPTION 'a plan change cannot apply before it takes effect';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_plan_change_guard BEFORE INSERT OR UPDATE OR DELETE ON team_plan_change
 FOR EACH ROW EXECUTE FUNCTION guard_team_plan_change();
CREATE TRIGGER team_plan_change_truncate_guard BEFORE TRUNCATE ON team_plan_change
 FOR EACH STATEMENT EXECUTE FUNCTION guard_team_plan_change();

ALTER TABLE team_plan_change ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON team_plan_change FROM PUBLIC;
DO $$ DECLARE r text; BEGIN
 FOREACH r IN ARRAY ARRAY['anon','authenticated','service_role'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('REVOKE ALL ON team_plan_change FROM %I',r);
   IF r='service_role' THEN
    GRANT SELECT,INSERT,UPDATE ON team_plan_change TO service_role;
    CREATE POLICY team_plan_change_backend ON team_plan_change TO service_role USING(true) WITH CHECK(true);
   END IF;
  END IF;
 END LOOP;
 REVOKE ALL ON FUNCTION guard_team_plan_change() FROM PUBLIC;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='service_role') THEN GRANT EXECUTE ON FUNCTION guard_team_plan_change() TO service_role; END IF;
END $$;
