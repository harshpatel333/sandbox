ALTER TABLE sandbox ADD COLUMN system_template boolean NOT NULL DEFAULT false;
ALTER TABLE sandbox_snapshot ADD COLUMN system_template boolean NOT NULL DEFAULT false;

UPDATE sandbox s SET system_template = true
FROM template t WHERE t.id = s.template_id AND t.team_id <> s.team_id;

UPDATE sandbox_snapshot ss SET system_template = s.system_template
FROM sandbox s WHERE s.id = ss.sandbox_id AND s.team_id = ss.team_id;

CREATE OR REPLACE FUNCTION guard_free_sandbox_shape() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f record;
BEGIN
 BEGIN
  PERFORM 1 FROM team WHERE id=NEW.team_id FOR SHARE NOWAIT;
 EXCEPTION WHEN lock_not_available THEN
  RAISE EXCEPTION 'team plan assignment is changing' USING ERRCODE='SS003';
 END;
 SELECT * INTO f FROM billing_free_plan(NEW.team_id);
 IF f.free_since IS NOT NULL AND ((f.max_vcpu IS NOT NULL AND NEW.vcpu_count>f.max_vcpu)
  OR (f.max_memory_mib IS NOT NULL AND NEW.memory_mib>f.max_memory_mib)
  OR (f.max_disk_mib IS NOT NULL AND NEW.disk_mib>f.max_disk_mib)) THEN
  RAISE EXCEPTION 'sandbox exceeds plan limits' USING ERRCODE='SS002',
   DETAIL=jsonb_build_object('max_vcpu',COALESCE(f.max_vcpu,0),'max_memory_mib',COALESCE(f.max_memory_mib,0),
    'max_disk_mib',COALESCE(f.max_disk_mib,0),'vcpu',NEW.vcpu_count,'memory_mib',NEW.memory_mib,'disk_mib',NEW.disk_mib)::text;
 END IF;
 IF f.free_since IS NOT NULL AND TG_OP = 'INSERT' AND NOT NEW.system_template THEN
  RAISE EXCEPTION 'Free supports only default and system templates' USING ERRCODE='SS004';
 END IF;
 RETURN NEW;
END $$;
