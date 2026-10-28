-- Every curated template now ships at 2 vCPU / 4096 MiB memory / 8192 MiB
-- disk. A sandbox inherits its template's shape and admission rejects a shape
-- above the team's plan caps, so the plans that sat below that shape move up
-- to it. Published catalog versions are immutable: each change is a new
-- published version that copies every other term from its predecessor.
--
--  1. v2 Free: new teams freeze its caps at signup.
--  2. Open v2 Free assignments: existing teams carry frozen caps, so each one
--     still below the shape closes and reopens on the new version. Teams whose
--     limits staff customized keep their team row untouched (apply_assignment_limits).
--  3. Legacy pay-as-you-go: legacy teams without caps of their own fall back
--     to its latest published version (team_effective_limits).
DO $$
DECLARE
 old billing_plan_version;
 free_id uuid; free_version integer;
 payg_id uuid; payg_version integer;
 a team_plan_assignment;
 at_time timestamptz;
 moved integer := 0;
BEGIN
 -- 1. v2 Free catalog.
 SELECT * INTO old FROM billing_plan_version
  WHERE family='free' AND pricing_version='v2' AND state='published' FOR UPDATE;
 IF FOUND AND (COALESCE(old.max_template_vcpu,0)<2 OR COALESCE(old.max_template_memory_mib,0)<4096
   OR COALESCE(old.max_template_disk_mib,0)<8192) THEN
  SELECT max(version)+1 INTO free_version FROM billing_plan_version WHERE family='free';
  free_id:=md5('billing-plan-version:v2:free:'||free_version)::uuid;
  INSERT INTO billing_plan_version SELECT (jsonb_populate_record(NULL::billing_plan_version,to_jsonb(old)||jsonb_build_object(
   'id',free_id,'version',free_version,'state','draft',
   'max_template_vcpu',GREATEST(old.max_template_vcpu,2),
   'max_template_memory_mib',GREATEST(old.max_template_memory_mib,4096),
   'max_template_disk_mib',GREATEST(old.max_template_disk_mib,8192),
   'created_by',NULL,'created_at',clock_timestamp(),'updated_at',clock_timestamp(),
   'published_by',NULL,'published_at',NULL,'retired_by',NULL,'retired_at',NULL))).*;
  PERFORM publish_billing_plan_version(free_id,NULL);
 END IF;
 SELECT id,version INTO free_id,free_version FROM billing_plan_version
  WHERE family='free' AND pricing_version='v2' AND state='published';

 -- 2. Open v2 Free assignments still below the shape.
 IF free_id IS NOT NULL THEN
  FOR a IN SELECT p.* FROM team_plan_assignment p
   WHERE p.kind='free' AND p.plan_key='free' AND p.effective_to IS NULL AND p.contract_id IS NULL
    AND p.terms->>'pricing_version'='v2'
    AND (COALESCE(billing_json_cap(p.limits->'max_vcpu'),0)<2
     OR COALESCE(billing_json_cap(p.limits->'max_memory_mib'),0)<4096
     OR COALESCE(billing_json_cap(p.limits->'max_disk_mib'),0)<8192)
   ORDER BY p.team_id
  LOOP
   -- One team's frozen invoice history must not hold back every other team;
   -- such a team keeps its old caps and is reported for a manual follow-up.
   BEGIN
    PERFORM 1 FROM team WHERE id=a.team_id FOR UPDATE;
    at_time:=clock_timestamp();
    UPDATE team_plan_assignment SET effective_to=at_time WHERE id=a.id;
    INSERT INTO team_plan_assignment(team_id,plan_key,kind,plan_version_id,terms,limits,effective_from,source,reason,idempotency_key)
    VALUES(a.team_id,a.plan_key,a.kind,free_id,
     a.terms||jsonb_build_object('version_id',free_id,'version',free_version),
     a.limits||jsonb_build_object(
      'max_vcpu',GREATEST(COALESCE(billing_json_cap(a.limits->'max_vcpu'),0),2),
      'max_memory_mib',GREATEST(COALESCE(billing_json_cap(a.limits->'max_memory_mib'),0),4096),
      'max_disk_mib',GREATEST(COALESCE(billing_json_cap(a.limits->'max_disk_mib'),0),8192)),
     at_time,'migration','Free shape raised to 2 vCPU / 4096 MiB / 8192 MiB','free-shape-2-4-8:'||a.id);
    moved:=moved+1;
   EXCEPTION WHEN OTHERS THEN
    RAISE WARNING 'free shape: team % kept its Free caps: %',a.team_id,SQLERRM;
   END;
  END LOOP;
 END IF;
 RAISE NOTICE 'free shape: % open Free assignments moved to version %',moved,free_version;

 -- 3. Legacy pay-as-you-go fallback.
 SELECT * INTO old FROM billing_plan_version
  WHERE family='payg' AND pricing_version='legacy' AND kind='legacy_payg' AND state='published' FOR UPDATE;
 IF FOUND AND (COALESCE(old.max_template_vcpu,0)<2 OR COALESCE(old.max_template_memory_mib,0)<4096
   OR COALESCE(old.max_template_disk_mib,0)<8192) THEN
  SELECT max(version)+1 INTO payg_version FROM billing_plan_version WHERE family='payg';
  payg_id:=md5('billing-plan-version:legacy:payg:'||payg_version)::uuid;
  INSERT INTO billing_plan_version SELECT (jsonb_populate_record(NULL::billing_plan_version,to_jsonb(old)||jsonb_build_object(
   'id',payg_id,'version',payg_version,'state','draft',
   'max_template_vcpu',GREATEST(old.max_template_vcpu,2),
   'max_template_memory_mib',GREATEST(old.max_template_memory_mib,4096),
   'max_template_disk_mib',GREATEST(old.max_template_disk_mib,8192),
   'created_by',NULL,'created_at',clock_timestamp(),'updated_at',clock_timestamp(),
   'published_by',NULL,'published_at',NULL,'retired_by',NULL,'retired_at',NULL))).*;
  PERFORM publish_billing_plan_version(payg_id,NULL);
 END IF;
END $$;
