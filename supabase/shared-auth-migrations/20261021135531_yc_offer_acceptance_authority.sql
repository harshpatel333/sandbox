-- Shared Auth only. Acceptance is separate from approval and provider execution.
BEGIN;
-- These reachable predecessors otherwise let session temporary tables replace
-- the claim, acceptance fence, audit view or approval outbox. Preserve bodies/ACLs.
ALTER FUNCTION public.yc_claim_rpc(text,jsonb) SET search_path=pg_catalog,public,auth,pg_temp;
ALTER FUNCTION public.yc_staff_claim_rpc(text,jsonb) SET search_path=pg_catalog,public,auth,pg_temp;
ALTER FUNCTION public.yc_claim_approval_mail_rpc(text,jsonb) SET search_path=pg_catalog,public,auth,pg_temp;
ALTER FUNCTION public.yc_claim_view(public.yc_claim) SET search_path=pg_catalog,public,pg_temp;
ALTER FUNCTION public.reserve_yc_activation_authority(text,uuid,text,uuid,text,text,uuid) SET search_path=pg_catalog,public,pg_temp;
ALTER FUNCTION public.lock_yc_activation_authority(uuid,uuid,text) SET search_path=pg_catalog,public,pg_temp;

CREATE TABLE public.yc_preferred_identity_review (
 request_id uuid PRIMARY KEY,
 claim_id uuid NOT NULL UNIQUE REFERENCES public.yc_claim(id),
 revision bigint NOT NULL,
 user_id uuid NOT NULL,
 email text NOT NULL,
 team_id uuid NOT NULL UNIQUE,
 home_region text NOT NULL,
 company_identity text NOT NULL,
 verification_reference text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ready boolean NOT NULL DEFAULT false,
 approved_revision bigint,
 FOREIGN KEY(claim_id,revision) REFERENCES public.yc_claim_decision(claim_id,revision),
 FOREIGN KEY(claim_id,approved_revision) REFERENCES public.yc_claim_decision(claim_id,revision)
);

CREATE FUNCTION public.yc_preferred_identity_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
 IF TG_OP='DELETE' OR (to_jsonb(NEW)-ARRAY['ready','approved_revision']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['ready','approved_revision'])
 OR (OLD.ready AND NOT NEW.ready) OR (OLD.approved_revision IS NOT NULL AND NEW.approved_revision IS DISTINCT FROM OLD.approved_revision) THEN
  RAISE EXCEPTION 'preferred identity evidence is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER yc_preferred_identity_immutable BEFORE UPDATE OR DELETE ON public.yc_preferred_identity_review
 FOR EACH ROW EXECUTE FUNCTION public.yc_preferred_identity_immutable();

CREATE FUNCTION public.yc_preferred_claim_view(c public.yc_claim) RETURNS jsonb
LANGUAGE sql STABLE SET search_path=pg_catalog,public,pg_temp AS $$
 SELECT public.yc_claim_view(c)||jsonb_build_object('preferred_identity_review',
  (SELECT jsonb_build_object('review_request_id',r.request_id,'review_revision',r.revision,'user_id',r.user_id,
   'email',r.email,'team_id',r.team_id,'home_region',r.home_region,'company_identity',r.company_identity,
   'ready',r.ready,'approved_revision',r.approved_revision) FROM public.yc_preferred_identity_review r WHERE r.claim_id=c.id))
$$;

CREATE FUNCTION public.yc_preferred_claim_rpc(op text,p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,auth,pg_temp AS $$
DECLARE c public.yc_claim; r public.yc_preferred_identity_review; u record;
BEGIN
 IF p IS NULL OR jsonb_typeof(p)<>'object' OR op IS NULL OR op NOT IN ('lookup','prepare','review') THEN
  RAISE EXCEPTION 'invalid preferred identity request' USING ERRCODE='22023';
 END IF;
 IF op='lookup' THEN
  SELECT * INTO u FROM auth.users WHERE id=(p->>'user_id')::uuid;
  IF NOT FOUND OR u.deleted_at IS NOT NULL OR u.email_confirmed_at IS NULL THEN RETURN '{"claims":[]}'::jsonb; END IF;
  RETURN jsonb_build_object('claims',COALESCE((SELECT jsonb_agg(public.yc_preferred_claim_view(q)) FROM
   (SELECT qc.* FROM public.yc_claim qc WHERE
    (qc.submitted_email=lower(btrim(u.email)) AND (qc.claimed_user_id IS NULL OR qc.claimed_user_id=u.id) AND
     NOT EXISTS(SELECT 1 FROM public.yc_preferred_identity_review qr WHERE qr.claim_id=qc.id AND qr.user_id<>u.id))
    OR EXISTS(SELECT 1 FROM public.yc_preferred_identity_review qr WHERE qr.claim_id=qc.id AND qr.user_id=u.id AND qr.email=lower(btrim(u.email))
     AND NOT EXISTS(SELECT 1 FROM public.yc_claim_identity_history WHERE email_hash=encode(sha256(convert_to(qr.email,'UTF8')),'hex')))
    ORDER BY qc.created_at DESC LIMIT 20) q),'[]'::jsonb));
 END IF;
 SELECT * INTO c FROM public.yc_claim WHERE id=(p->>'id')::uuid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'claim unavailable' USING ERRCODE='P0002'; END IF;
 SELECT * INTO u FROM auth.users WHERE id=(p->>'user_id')::uuid FOR SHARE;
 IF NOT FOUND OR u.deleted_at IS NOT NULL OR u.email_confirmed_at IS NULL OR u.created_at<c.created_at
 OR c.lifecycle_changed OR c.state IN ('bound','rejected','revoked') OR c.claimed_user_id IS NOT NULL
 OR (p->>'team_created_at')::timestamptz IS NULL OR (p->>'team_created_at')::timestamptz<c.created_at
 OR COALESCE((p->>'team_eligible')::boolean,false) IS NOT TRUE
 OR p->>'verification_reference' IS DISTINCT FROM c.verification_reference
 OR p->>'company_identity' IS DISTINCT FROM c.company_identity OR COALESCE(c.company_identity,'')=''
 OR EXISTS(SELECT 1 FROM public.yc_claim_identity_history WHERE email_hash IN
  (encode(sha256(convert_to(lower(btrim(u.email)),'UTF8')),'hex'),encode(sha256(convert_to(c.submitted_email,'UTF8')),'hex')))
 OR EXISTS(SELECT 1 FROM public.yc_company_activation_authority WHERE company_identity=c.company_identity OR team_id=(p->>'team_id')::uuid) THEN
  RAISE EXCEPTION 'preferred account requires review' USING ERRCODE='22023';
 END IF;
 IF c.intent_team_id IS NOT NULL AND (c.intent_user_id IS DISTINCT FROM u.id OR c.intent_team_id IS DISTINCT FROM (p->>'team_id')::uuid OR c.intent_region IS DISTINCT FROM p->>'home_region') THEN
  RAISE EXCEPTION 'claim intent conflict' USING ERRCODE='23505';
 END IF;
 SELECT * INTO r FROM public.yc_preferred_identity_review WHERE claim_id=c.id;
 IF NOT FOUND THEN
  IF op<>'prepare' THEN RAISE EXCEPTION 'review preparation required' USING ERRCODE='22023'; END IF;
  UPDATE public.yc_claim SET state='manual_review',reason='preferred_identity_review',revision=revision+1,
   intent_user_id=u.id,intent_team_id=(p->>'team_id')::uuid,intent_region=p->>'home_region' WHERE id=c.id RETURNING * INTO c;
  INSERT INTO public.yc_claim_decision(claim_id,revision,decision,actor_id,reason,evidence_reference)
   VALUES(c.id,c.revision,'manual_review',u.id,'preferred_identity_review',c.verification_reference);
  INSERT INTO public.yc_preferred_identity_review(request_id,claim_id,revision,user_id,email,team_id,home_region,company_identity,verification_reference)
   VALUES((p->>'request_id')::uuid,c.id,c.revision,u.id,lower(btrim(u.email)),(p->>'team_id')::uuid,p->>'home_region',c.company_identity,c.verification_reference)
   RETURNING * INTO r;
 END IF;
 IF r.request_id IS DISTINCT FROM (p->>'request_id')::uuid OR r.user_id<>u.id OR r.email<>lower(btrim(u.email))
 OR r.team_id IS DISTINCT FROM (p->>'team_id')::uuid OR r.home_region IS DISTINCT FROM p->>'home_region' THEN
  RAISE EXCEPTION 'review identity changed' USING ERRCODE='23505';
 END IF;
 IF op='review' THEN UPDATE public.yc_preferred_identity_review SET ready=true WHERE request_id=r.request_id; END IF;
 RETURN jsonb_build_object('id',c.id,'claim_id',c.id,'state',CASE WHEN op='prepare' THEN 'review_prepared' ELSE 'review_required' END,
  'team_id',r.team_id,'home_region',r.home_region,'review_request_id',r.request_id,'review_revision',r.revision);
END $$;

CREATE FUNCTION public.yc_preferred_staff_rpc(op text,p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,auth,pg_temp AS $$
DECLARE c public.yc_claim; r public.yc_preferred_identity_review; u record; prior public.yc_staff_claim_request;
 canonical jsonb; approval uuid; result jsonb;
BEGIN
 IF op='list' THEN
  result:=public.yc_staff_claim_rpc(op,p);
  RETURN jsonb_build_object('claims',COALESCE((SELECT jsonb_agg(public.yc_preferred_claim_view(qc)) FROM jsonb_array_elements(result->'claims') j
   JOIN public.yc_claim qc ON qc.id=(j->>'id')::uuid),'[]'::jsonb));
 ELSIF op='operator-get' THEN
  SELECT * INTO c FROM public.yc_claim WHERE id=(p->>'id')::uuid;
  IF NOT FOUND THEN RAISE EXCEPTION 'claim unavailable' USING ERRCODE='P0002'; END IF;
  RETURN public.yc_claim_rpc(op,p)||public.yc_preferred_claim_view(c);
 ELSIF p->>'review_request_id' IS NULL THEN
  IF op IN ('approve','prepare-approve') AND EXISTS(SELECT 1 FROM public.yc_preferred_identity_review WHERE claim_id=(p->>'id')::uuid) THEN
   RAISE EXCEPTION 'exact preferred review decision required' USING ERRCODE='22023';
  END IF;
  RETURN public.yc_staff_claim_rpc(op,p);
 END IF;
 IF op NOT IN ('prepare-approve','approve') OR p->>'actor_id' IS NULL OR p->>'request_id' IS NULL OR p->>'expected_revision' IS NULL
 OR btrim(COALESCE(p->>'reason',''))='' OR btrim(COALESCE(p->>'evidence_reference',''))='' THEN
  RAISE EXCEPTION 'audited preferred review decision required' USING ERRCODE='22023';
 END IF;
 SELECT * INTO c FROM public.yc_claim WHERE id=(p->>'id')::uuid FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'claim unavailable' USING ERRCODE='P0002'; END IF;
 canonical:=p-ARRAY['team_created_at','team_eligible'];
 SELECT * INTO prior FROM public.yc_staff_claim_request WHERE request_id=(p->>'request_id')::uuid;
 IF FOUND THEN
  IF prior.claim_id<>c.id OR prior.parameters<>canonical OR prior.decision<>'approve' THEN RAISE EXCEPTION 'decision identity changed' USING ERRCODE='23505'; END IF;
  RETURN public.yc_preferred_claim_view(c);
 END IF;
 SELECT * INTO r FROM public.yc_preferred_identity_review WHERE claim_id=c.id;
 IF NOT FOUND OR NOT r.ready OR r.request_id IS DISTINCT FROM (p->>'review_request_id')::uuid OR r.revision IS DISTINCT FROM (p->>'review_revision')::bigint
 OR r.user_id IS DISTINCT FROM (p->>'user_id')::uuid OR r.team_id IS DISTINCT FROM (p->>'team_id')::uuid
 OR r.home_region IS DISTINCT FROM p->>'home_region' OR r.approved_revision IS NOT NULL OR c.revision IS DISTINCT FROM (p->>'expected_revision')::bigint
 OR c.state<>'manual_review' OR c.lifecycle_changed OR c.intent_user_id IS DISTINCT FROM r.user_id
 OR c.intent_team_id IS DISTINCT FROM r.team_id OR c.intent_region IS DISTINCT FROM r.home_region
 OR c.company_identity IS DISTINCT FROM r.company_identity OR (p->>'company_identity' IS NOT NULL AND p->>'company_identity'<>r.company_identity)
 OR COALESCE((p->>'team_eligible')::boolean,false) IS NOT TRUE OR (p->>'team_created_at')::timestamptz IS NULL
 OR (p->>'team_created_at')::timestamptz<c.created_at THEN
  RAISE EXCEPTION 'preferred review changed' USING ERRCODE='23505';
 END IF;
 SELECT * INTO u FROM auth.users WHERE id=r.user_id FOR SHARE;
 IF NOT FOUND OR u.deleted_at IS NOT NULL OR u.email_confirmed_at IS NULL OR lower(btrim(u.email))<>r.email OR u.created_at<c.created_at
 OR EXISTS(SELECT 1 FROM public.yc_claim_identity_history WHERE email_hash IN
  (encode(sha256(convert_to(r.email,'UTF8')),'hex'),encode(sha256(convert_to(c.submitted_email,'UTF8')),'hex'))) THEN
  RAISE EXCEPTION 'preferred account changed' USING ERRCODE='22023';
 END IF;
 IF op='prepare-approve' THEN RETURN public.yc_preferred_claim_view(c); END IF;
 IF EXISTS(SELECT 1 FROM public.yc_company_activation_authority WHERE company_identity=r.company_identity OR team_id=r.team_id) THEN
  RAISE EXCEPTION 'existing authority requires review' USING ERRCODE='23505';
 END IF;
 SELECT approval_id INTO approval FROM public.reserve_yc_activation_authority(r.company_identity,r.team_id,r.home_region,(p->>'actor_id')::uuid,'operator',p->>'evidence_reference',c.id);
 UPDATE public.yc_claim SET state='bound',reason='activation_pending',claimed_user_id=r.user_id,claimed_team_id=r.team_id,
  home_region=r.home_region,claimed_at=clock_timestamp(),approval_id=approval,acceptance_required=true,revision=revision+1
  WHERE id=c.id RETURNING * INTO c;
 INSERT INTO public.yc_claim_decision(claim_id,revision,decision,actor_id,reason,evidence_reference)
  VALUES(c.id,c.revision,'operator',(p->>'actor_id')::uuid,p->>'reason',p->>'evidence_reference');
 UPDATE public.yc_preferred_identity_review SET approved_revision=c.revision WHERE request_id=r.request_id;
 INSERT INTO public.yc_claim_approval_mail(claim_id,decision_revision,approved_email,approved_user_id,approved_team_id,home_region,approval_id)
  VALUES(c.id,c.revision,r.email,r.user_id,r.team_id,r.home_region,approval);
 INSERT INTO public.yc_staff_claim_request(request_id,claim_id,parameters,decision,decision_revision)
  VALUES((p->>'request_id')::uuid,c.id,canonical,'approve',c.revision);
 RETURN public.yc_preferred_claim_view(c);
END $$;

CREATE TABLE public.yc_offer_acceptance_authority (
 acceptance_id uuid PRIMARY KEY,
 claim_id uuid NOT NULL UNIQUE REFERENCES public.yc_claim(id),
 team_id uuid NOT NULL UNIQUE,
 user_id uuid NOT NULL,
 company_identity text NOT NULL UNIQUE,
 home_region text NOT NULL,
 approval_id uuid NOT NULL UNIQUE,
 approval_revision bigint NOT NULL,
 offer_key text NOT NULL,
 offer_version integer NOT NULL CHECK (offer_version>0),
 terms_digest text NOT NULL CHECK (terms_digest ~ '^[a-f0-9]{64}$'),
 renewal_terms_digest text NOT NULL CHECK (renewal_terms_digest ~ '^[a-f0-9]{64}$'),
 request_id uuid NOT NULL UNIQUE,
 accepted_at timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 assignment_id uuid UNIQUE,
 FOREIGN KEY(claim_id,approval_revision) REFERENCES public.yc_claim_decision(claim_id,revision)
);

CREATE FUNCTION public.yc_offer_acceptance_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$ BEGIN
 IF TG_OP='DELETE' OR (to_jsonb(NEW)-'assignment_id') IS DISTINCT FROM (to_jsonb(OLD)-'assignment_id')
 OR OLD.assignment_id IS NOT NULL OR NEW.assignment_id IS NULL THEN
  RAISE EXCEPTION 'acceptance history is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER yc_offer_acceptance_immutable BEFORE UPDATE OR DELETE ON public.yc_offer_acceptance_authority
 FOR EACH ROW EXECUTE FUNCTION public.yc_offer_acceptance_immutable();

-- Dispatch never locks a claim. Revocation takes claim then authority; Auth
-- lifecycle takes only the user and its history, so user then authority is safe.
CREATE FUNCTION public.yc_offer_acceptance_rpc(op text,p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,auth,pg_temp AS $$
DECLARE c public.yc_claim; a public.yc_company_activation_authority;
 r public.yc_offer_acceptance_authority; wanted public.yc_offer_acceptance_authority; u record; revision bigint; approved_email text; preferred_identity boolean;
BEGIN
 IF p IS NULL OR jsonb_typeof(p)<>'object' OR op IS NULL OR op NOT IN ('offer','accept','admit','guard','link') THEN
  RAISE EXCEPTION 'invalid acceptance request' USING ERRCODE='22023';
 END IF;
 SELECT * INTO c FROM public.yc_claim WHERE id=(p->>'claim_id')::uuid;
 IF NOT FOUND THEN RAISE EXCEPTION 'claim unavailable' USING ERRCODE='P0002'; END IF;
 SELECT email INTO approved_email FROM public.yc_preferred_identity_review WHERE claim_id=c.id AND user_id=c.claimed_user_id
  AND team_id=c.claimed_team_id AND ready AND approved_revision IS NOT NULL;
 preferred_identity:=FOUND;
 approved_email:=COALESCE(approved_email,c.submitted_email);
 SELECT * INTO u FROM auth.users WHERE id=(p->>'user_id')::uuid FOR SHARE;
 IF NOT FOUND OR u.deleted_at IS NOT NULL OR u.email_confirmed_at IS NULL OR u.created_at<c.created_at
 OR lower(btrim(u.email)) IS DISTINCT FROM approved_email OR c.lifecycle_changed
 -- Explicit staff approval makes the preferred identity authoritative. Original
 -- submission history remains evidence, not a fence on an unrelated later account.
 OR EXISTS(SELECT 1 FROM public.yc_claim_identity_history WHERE
  email_hash=encode(sha256(convert_to(approved_email,'UTF8')),'hex') OR
  (NOT preferred_identity AND email_hash=encode(sha256(convert_to(c.submitted_email,'UTF8')),'hex'))) THEN
  RAISE EXCEPTION 'account requires review' USING ERRCODE='22023';
 END IF;
 SELECT * INTO a FROM public.yc_company_activation_authority WHERE company_identity=c.company_identity FOR SHARE;
 IF NOT FOUND OR a.revoked_at IS NOT NULL THEN RAISE EXCEPTION 'approval unavailable' USING ERRCODE='22023'; END IF;
 -- Read again after the serialization point, including a revoke that committed
 -- while the authority lock was being acquired. Notification revisions differ.
 SELECT * INTO STRICT c FROM public.yc_claim WHERE id=c.id;
 SELECT d.revision INTO revision FROM public.yc_claim_decision d WHERE d.claim_id=c.id
  AND d.decision IN ('operator','verified_claim') ORDER BY d.revision DESC LIMIT 1;
 IF c.state<>'bound' OR c.claimed_user_id IS DISTINCT FROM u.id OR c.claimed_team_id IS DISTINCT FROM (p->>'team_id')::uuid
 OR c.home_region IS DISTINCT FROM p->>'home_region' OR a.team_id IS DISTINCT FROM c.claimed_team_id
 OR a.home_region IS DISTINCT FROM c.home_region OR a.approval_id IS DISTINCT FROM c.approval_id
 OR a.claim_id IS DISTINCT FROM c.id OR revision IS NULL THEN
  RAISE EXCEPTION 'claim binding requires review' USING ERRCODE='22023';
 END IF;
 IF op='offer' THEN
  RETURN jsonb_build_object('claim_id',c.id,'user_id',u.id,'team_id',c.claimed_team_id,'home_region',c.home_region,
   'company_identity',c.company_identity,'approval_id',a.approval_id,'approval_revision',revision,
   'offer_key',a.offer_key,'offer_version',a.offer_version,'claim_created_at',c.created_at);
 END IF;
 IF (p->>'approval_id')::uuid IS DISTINCT FROM a.approval_id OR (p->>'approval_revision')::bigint IS DISTINCT FROM revision
 OR p->>'company_identity' IS DISTINCT FROM a.company_identity OR p->>'offer_key' IS DISTINCT FROM a.offer_key
 OR (p->>'offer_version')::integer IS DISTINCT FROM a.offer_version THEN
  RAISE EXCEPTION 'accepted authority changed' USING ERRCODE='23505';
 END IF;
 wanted:=jsonb_populate_record(NULL::public.yc_offer_acceptance_authority,p);
 IF op='accept' THEN
  IF (p->>'accepted_at')::timestamptz IS NULL OR (p->>'accepted_at')::timestamptz>clock_timestamp()+interval '1 minute'
   OR (p->>'accepted_at')::timestamptz<c.claimed_at THEN RAISE EXCEPTION 'invalid acceptance time' USING ERRCODE='22023'; END IF;
  INSERT INTO public.yc_offer_acceptance_authority(acceptance_id,claim_id,team_id,user_id,company_identity,home_region,approval_id,approval_revision,offer_key,offer_version,terms_digest,renewal_terms_digest,request_id,accepted_at)
  VALUES((p->>'acceptance_id')::uuid,c.id,c.claimed_team_id,u.id,c.company_identity,c.home_region,a.approval_id,revision,a.offer_key,a.offer_version,p->>'terms_digest',p->>'renewal_terms_digest',(p->>'request_id')::uuid,(p->>'accepted_at')::timestamptz)
  ON CONFLICT DO NOTHING;
 END IF;
 SELECT * INTO r FROM public.yc_offer_acceptance_authority WHERE acceptance_id=(p->>'acceptance_id')::uuid;
 IF NOT FOUND OR (to_jsonb(r)-ARRAY['recorded_at','assignment_id']) IS DISTINCT FROM (to_jsonb(wanted)-ARRAY['recorded_at','assignment_id']) THEN
  RAISE EXCEPTION 'acceptance identity changed' USING ERRCODE='23505';
 END IF;
 IF op='link' THEN
  IF p->>'assignment_id' IS NULL THEN RAISE EXCEPTION 'assignment required' USING ERRCODE='22023'; END IF;
  UPDATE public.yc_offer_acceptance_authority SET assignment_id=(p->>'assignment_id')::uuid
   WHERE acceptance_id=r.acceptance_id AND assignment_id IS NULL;
  SELECT * INTO STRICT r FROM public.yc_offer_acceptance_authority WHERE acceptance_id=r.acceptance_id;
 END IF;
 IF op IN ('guard','link') AND r.assignment_id IS DISTINCT FROM (p->>'assignment_id')::uuid THEN
  RAISE EXCEPTION 'assignment identity changed' USING ERRCODE='23505';
 END IF;
 RETURN to_jsonb(r);
END $$;

ALTER TABLE public.yc_offer_acceptance_authority ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.yc_preferred_identity_review ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON public.yc_offer_acceptance_authority,public.yc_preferred_identity_review FROM PUBLIC;
REVOKE ALL ON FUNCTION public.yc_offer_acceptance_immutable(),public.yc_offer_acceptance_rpc(text,jsonb),public.yc_preferred_identity_immutable(),public.yc_preferred_claim_view(public.yc_claim),public.yc_preferred_claim_rpc(text,jsonb),public.yc_preferred_staff_rpc(text,jsonb) FROM PUBLIC;
DO $$ DECLARE role_name text; BEGIN
 FOREACH role_name IN ARRAY ARRAY['anon','authenticated','service_role','promotion_evidence_proxy'] LOOP
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=role_name) THEN
   EXECUTE format('REVOKE ALL ON public.yc_offer_acceptance_authority,public.yc_preferred_identity_review FROM %I',role_name);
   EXECUTE format('REVOKE ALL ON FUNCTION public.yc_offer_acceptance_immutable(),public.yc_offer_acceptance_rpc(text,jsonb),public.yc_preferred_identity_immutable(),public.yc_preferred_claim_view(public.yc_claim),public.yc_preferred_claim_rpc(text,jsonb),public.yc_preferred_staff_rpc(text,jsonb) FROM %I',role_name);
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname='promotion_evidence_proxy') THEN
  GRANT EXECUTE ON FUNCTION public.yc_offer_acceptance_rpc(text,jsonb),public.yc_preferred_claim_rpc(text,jsonb),public.yc_preferred_staff_rpc(text,jsonb) TO promotion_evidence_proxy;
 END IF;
END $$;
COMMIT;
