-- Session-local operator helpers, never installed as application RPCs.
-- Load only inside BEGIN; dry-run with ROLLBACK. See free-zero-templates.md.
CREATE FUNCTION pg_temp.publish_free_zero_templates(p_from uuid,p_new uuid,p_actor uuid,p_reason text,p_zero_aware_console_verified boolean)
RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE old billing_plan_version; fresh billing_plan_version; payload jsonb;
BEGIN
 IF p_zero_aware_console_verified IS DISTINCT FROM true THEN RAISE EXCEPTION 'deploy and verify zero-aware console first'; END IF;
 IF p_actor IS NULL OR NOT EXISTS(SELECT 1 FROM profile WHERE id=p_actor) OR p_reason IS NULL OR length(btrim(p_reason)) NOT BETWEEN 1 AND 500 THEN
  RAISE EXCEPTION 'operator profile and audit reason required';
 END IF;
 PERFORM 1 FROM billing_pricing_switch FOR SHARE;
 PERFORM pg_advisory_xact_lock(hashtext('billing-catalog:free:v2')::bigint);
 SELECT * INTO STRICT old FROM billing_plan_version WHERE id=p_from;
 IF old.family<>'free' OR old.pricing_version<>'v2' OR old.kind<>'free' OR old.max_templates IS DISTINCT FROM 10 THEN
  RAISE EXCEPTION 'expected original v2 Free ten-template version';
 END IF;
 payload:=to_jsonb(old)-ARRAY['id','version','state','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at','max_templates'];
 SELECT * INTO fresh FROM billing_plan_version WHERE id=p_new;
 IF FOUND THEN
  IF fresh.state<>'published' OR fresh.max_templates IS DISTINCT FROM 0 OR
   to_jsonb(fresh)-ARRAY['id','version','state','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at','max_templates'] IS DISTINCT FROM payload
   OR fresh.created_by IS DISTINCT FROM p_actor
   OR NOT EXISTS(SELECT 1 FROM billing_catalog_audit WHERE version_id=p_new AND actor_id=p_actor AND action='publish' AND reason=p_reason) THEN
   RAISE EXCEPTION 'replacement identity already used with different terms';
  END IF;
  RETURN p_new;
 END IF;
 IF old.state<>'published' THEN RAISE EXCEPTION 'expected source is no longer published'; END IF;
 INSERT INTO billing_plan_version SELECT (jsonb_populate_record(NULL::billing_plan_version,to_jsonb(old)||jsonb_build_object(
  'id',p_new,'version',(SELECT max(version)+1 FROM billing_plan_version WHERE family='free'),
  'state','draft','max_templates',0,'created_by',p_actor,'created_at',clock_timestamp(),'updated_at',clock_timestamp(),
  'published_by',NULL,'published_at',NULL,'retired_by',NULL,'retired_at',NULL))).*;
 INSERT INTO billing_catalog_audit(actor_id,action,version_id,reason,terms)
 SELECT p_actor,'create_draft',id,p_reason,to_jsonb(v) FROM billing_plan_version v WHERE id=p_new;
 PERFORM publish_billing_plan_version(p_new,p_actor);
 INSERT INTO billing_catalog_audit(actor_id,action,version_id,reason,terms)
 SELECT p_actor,'publish',id,p_reason,to_jsonb(v) FROM billing_plan_version v WHERE id=p_new;
 RETURN p_new;
END $$;

CREATE FUNCTION pg_temp.transition_free_zero_templates(p_team uuid,p_assignment uuid,p_version uuid,p_actor uuid,p_reason text)
RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE prior team_plan_assignment; v billing_plan_version; previous billing_plan_version; replay team_plan_assignment;
 at_time timestamptz; result uuid; key text:='free-zero-templates:'||p_assignment::text;
BEGIN
 IF p_actor IS NULL OR NOT EXISTS(SELECT 1 FROM profile WHERE id=p_actor) OR p_reason IS NULL OR length(btrim(p_reason)) NOT BETWEEN 1 AND 500 THEN
  RAISE EXCEPTION 'operator profile and audit reason required';
 END IF;
 -- Same order as financial operations: invoice serialization, then team lock.
 PERFORM pg_advisory_xact_lock(hashtextextended('invoice:'||p_team::text,0));
 PERFORM 1 FROM team WHERE id=p_team FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'target team missing'; END IF;
 PERFORM 1 FROM team_billing_account WHERE team_id=p_team FOR UPDATE;
 SELECT * INTO STRICT prior FROM team_plan_assignment WHERE id=p_assignment AND team_id=p_team;
 SELECT * INTO STRICT v FROM billing_plan_version WHERE id=p_version FOR SHARE;
 SELECT * INTO STRICT previous FROM billing_plan_version WHERE id=prior.plan_version_id;
 IF prior.kind<>'free' OR prior.plan_key<>'free' OR prior.contract_id IS NOT NULL
  OR prior.source NOT IN ('signup','migration')
  OR previous.family<>'free' OR previous.pricing_version<>'v2'
  OR prior.terms->>'version_id' IS DISTINCT FROM previous.id::text
  OR prior.terms->>'kind' IS DISTINCT FROM 'free'
  OR prior.terms->>'pricing_version' IS DISTINCT FROM 'v2'
  OR v.family<>'free' OR v.pricing_version<>'v2' OR v.kind<>'free' OR v.state<>'published' OR v.max_templates IS DISTINCT FROM 0
  OR to_jsonb(v)-ARRAY['id','version','state','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at','max_templates']
    IS DISTINCT FROM to_jsonb(previous)-ARRAY['id','version','state','created_by','created_at','updated_at','published_by','published_at','retired_by','retired_at','max_templates']
  OR prior.limits->'max_templates' IS DISTINCT FROM '10'::jsonb THEN
  RAISE EXCEPTION 'target is not an unchanged ordinary v2 Free assignment';
 END IF;
 SELECT * INTO replay FROM team_plan_assignment WHERE team_id=p_team AND idempotency_key=key;
 IF FOUND THEN
  IF replay.plan_version_id<>p_version OR replay.assigned_by IS DISTINCT FROM p_actor OR replay.reason<>p_reason
   OR replay.effective_to IS NOT NULL OR prior.effective_to IS DISTINCT FROM replay.effective_from THEN
   RAISE EXCEPTION 'transition replay no longer matches current assignment';
  END IF;
  RETURN replay.id;
 END IF;
 IF prior.effective_to IS NOT NULL THEN RAISE EXCEPTION 'target assignment is no longer current'; END IF;
 IF EXISTS(SELECT 1 FROM team WHERE id=p_team AND (limits_customized OR max_templates IS NOT NULL))
  OR EXISTS(SELECT 1 FROM billing_commercial_contract WHERE team_id=p_team)
  OR EXISTS(SELECT 1 FROM billing_pro_upgrade WHERE team_id=p_team)
  OR EXISTS(SELECT 1 FROM billing_prepaid_checkout_operation WHERE team_id=p_team)
  OR EXISTS(SELECT 1 FROM billing_prepaid_purchase WHERE team_id=p_team)
  OR EXISTS(SELECT 1 FROM team_billing_account WHERE team_id=p_team AND
    (stripe_subscription_id IS NOT NULL OR commercial_billing_anchor IS NOT NULL OR checkout_initializing_at IS NOT NULL
     OR checkout_session_id IS NOT NULL OR checkout_may_exist OR checkout_request_key IS NOT NULL)) THEN
  RAISE EXCEPTION 'target has custom limits or financial history/fence; coordinate separately';
 END IF;
 at_time:=clock_timestamp();
 UPDATE team_plan_assignment SET effective_to=at_time WHERE id=p_assignment;
 INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,assigned_by,reason,idempotency_key)
 VALUES(p_team,'free','free',p_version,prior.terms||jsonb_build_object('version_id',v.id,'version',v.version),
  prior.limits||jsonb_build_object('max_templates',0),at_time,'staff',p_actor,p_reason,key) RETURNING id INTO result;
 RETURN result;
END $$;
