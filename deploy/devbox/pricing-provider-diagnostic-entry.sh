#!/usr/bin/env bash
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$here/services/common.sh"
load_box_config
[[ $DEVBOX_NAME == awille-ai-dev-billing && $DEVBOX_ID == 29baf003ad05 ]] || die "incorrect diagnostic target"
sha=$(git -C "$here/../.." rev-parse HEAD)
output="$DEVBOX_STATE_DIR/pro-qa/inventory-$sha.json"
if python3 "$here/pricing-pro-provider-diagnostic.py" --state-dir "$DEVBOX_STATE_DIR" \
  --source-receipt "$DEVBOX_STATE_DIR/pro-qa/calendar-4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b.json" \
  --output "$output"; then
  status=0
else
  status=$?
fi
if [[ -r $output ]]; then
  python3 - "$output" <<'PY'
import json,sys
from pathlib import Path
result=json.loads(Path(sys.argv[1]).read_text())
allowed=('status','source_sha256','original_request','fixtures','customer_subscription_ids','requests','error','subscription_id')
print(json.dumps({key:result[key] for key in allowed if key in result}))
PY
fi
die "provider diagnostic finished with status $status; no component deployment was attempted in this run"
