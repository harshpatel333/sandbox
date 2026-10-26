-- Keep the existing admission trigger and its privileges; every plan uses the same resolved caps.
CREATE OR REPLACE FUNCTION guard_free_sandbox_shape() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f record;
BEGIN
 BEGIN
  PERFORM 1 FROM team WHERE id=NEW.team_id FOR SHARE NOWAIT;
 EXCEPTION WHEN lock_not_available THEN
  RAISE EXCEPTION 'team plan assignment is changing' USING ERRCODE='SS003';
 END;
 SELECT * INTO f FROM team_effective_limits(NEW.team_id);
 IF NOT FOUND OR f.max_vcpu IS NULL OR f.max_memory_mib IS NULL OR f.max_disk_mib IS NULL
  OR f.max_vcpu<=0 OR f.max_memory_mib<=0 OR f.max_disk_mib<=0 THEN
  RAISE EXCEPTION 'team plan limits unavailable' USING ERRCODE='SS003';
 END IF;
 IF NEW.vcpu_count>f.max_vcpu OR NEW.memory_mib>f.max_memory_mib OR NEW.disk_mib>f.max_disk_mib THEN
  RAISE EXCEPTION 'sandbox exceeds plan limits' USING ERRCODE='SS002',
   DETAIL=jsonb_build_object('max_vcpu',COALESCE(f.max_vcpu,0),'max_memory_mib',COALESCE(f.max_memory_mib,0),
    'max_disk_mib',COALESCE(f.max_disk_mib,0),'vcpu',NEW.vcpu_count,'memory_mib',NEW.memory_mib,'disk_mib',NEW.disk_mib)::text;
 END IF;
 -- Preserve Free frozen-cap fallback for partially customized team limits.
 SELECT * INTO f FROM billing_free_plan(NEW.team_id);
 IF FOUND AND ((f.max_vcpu IS NOT NULL AND NEW.vcpu_count>f.max_vcpu)
  OR (f.max_memory_mib IS NOT NULL AND NEW.memory_mib>f.max_memory_mib)
  OR (f.max_disk_mib IS NOT NULL AND NEW.disk_mib>f.max_disk_mib)) THEN
  RAISE EXCEPTION 'sandbox exceeds plan limits' USING ERRCODE='SS002',
   DETAIL=jsonb_build_object('max_vcpu',COALESCE(f.max_vcpu,0),'max_memory_mib',COALESCE(f.max_memory_mib,0),
    'max_disk_mib',COALESCE(f.max_disk_mib,0),'vcpu',NEW.vcpu_count,'memory_mib',NEW.memory_mib,'disk_mib',NEW.disk_mib)::text;
 END IF;
 RETURN NEW;
END $$;
