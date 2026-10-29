-- A subscription that ended in Stripe moves its team to Free through a plan
-- change no customer asked for, so pricing admission never gates it.
ALTER TABLE team_plan_change ADD COLUMN provider_ended boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT team_plan_change_provider_ended CHECK(NOT provider_ended
  OR (target_kind='free' AND pauses_running AND subscription_id IS NOT NULL AND requested_via='staff'));

CREATE OR REPLACE FUNCTION billing_guard_cohort_plan_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' AND EXISTS(SELECT 1 FROM billing_new_team_enrollment WHERE team_id=NEW.team_id) THEN
  IF NEW.terminal_undo_of IS NULL THEN
   IF NEW.target_terms->>'pricing_version' IS DISTINCT FROM 'v2' THEN
    RAISE EXCEPTION 'enrolled plan change requires frozen v2 terms' USING ERRCODE='55000';
   END IF;
   IF NOT NEW.provider_ended THEN
    PERFORM billing_check_cohort_admission(NEW.team_id,NEW.admission_source,NEW.admission_epoch);
   END IF;
  ELSIF NOT EXISTS(SELECT 1 FROM team_plan_change p WHERE p.id=NEW.terminal_undo_of AND p.team_id=NEW.team_id
    AND p.admission_source IS NOT DISTINCT FROM NEW.admission_source AND p.admission_epoch IS NOT DISTINCT FROM NEW.admission_epoch) THEN
   RAISE EXCEPTION 'terminal undo must retain accepted plan admission' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
