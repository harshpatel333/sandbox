#!/usr/bin/env bash
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$here/services/common.sh"
load_box_config
load_local_secrets
[[ ${DEVBOX_NAME:-} == awille-ai-dev-billing && ${DEVBOX_ID:-} == 29baf003ad05 ]] || die "incorrect integration target"
[[ $DEVBOX_DATA_DIR == /mnt/sandbox-data/superserve-devbox ]] || die "unexpected data directory"
marker="$DEVBOX_STATE_DIR/pricing-integration-reset.json"
[[ ! -e $marker ]] || exit 0
stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup="$DEVBOX_DATA_DIR/pricing-integration-backup-$stamp"
install -d -m 0700 "$backup"
python3 - "$backup" <<'PYCODE'
import hashlib,json,subprocess,sys
from pathlib import Path
backup=Path(sys.argv[1])
pid=int(subprocess.check_output(['systemctl','show','superserve-devbox-api.service','--property=MainPID','--value'],text=True))
exe=Path('/proc')/str(pid)/'exe'
record={'pid':pid,'executable':str(exe.resolve()),'binary_sha256':hashlib.sha256(exe.read_bytes()).hexdigest(),'original_api_deploy_receipt_present':Path('/var/lib/superserve-devbox/api-deploy.json').exists(),'receipt_note':'The workflow removes api-deploy.json before invoking selected scripts; retained prior workflow logs are separate from this live process observation.'}
(backup/'previous-api-runtime.json').write_text(json.dumps(record)+'\n')
PYCODE
cp "$DEVBOX_STATE_DIR/runtime.json" "$backup/previous-runtime.json"
compose exec -T db psql -X -At -U supabase_admin -d postgres -p 54322 -v ON_ERROR_STOP=1 -c "SELECT jsonb_build_object('versions',(SELECT jsonb_agg(version ORDER BY version) FROM supabase_migrations.schema_migrations),'team_count',(SELECT count(*) FROM team),'sandbox_count',(SELECT count(*) FROM sandbox),'pricing_switch_installed',to_regclass('public.billing_pricing_switch') IS NOT NULL);" > "$backup/previous-history.json"
systemctl stop superserve-devbox-console.service superserve-devbox-api.service superserve-vmd
compose exec -T db pg_dump -U supabase_admin -d postgres -p 54322 -Fc > "$backup/previous-database.dump"
[[ -s $backup/previous-database.dump ]] || die "empty database backup"
compose down
mv "$DEVBOX_DATA_DIR/postgres" "$backup/postgres"
install -d -m 0700 "$DEVBOX_DATA_DIR/postgres"
"$here/services-up.sh"
python3 - "$marker" "$backup" <<'PY'
import json,sys
from pathlib import Path
Path(sys.argv[1]).write_text(json.dumps({'target':'awille-ai-dev-billing','backup':sys.argv[2],'fresh_database':True})+'\n')
PY
log "fresh integration database prepared; prior history and database retained in root-only backup"
