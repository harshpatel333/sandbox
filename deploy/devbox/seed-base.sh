#!/usr/bin/env bash
# Make this box's host schedulable and build the curated base template, so
# teams that sign up on the box can create sandboxes. Runs once at create,
# after install-api.sh, install-vmd.sh and install-proxy.sh. Safe to re-run:
# an active host and an unchanged, already-built template are left alone.
#
#   seed-base.sh [--dry-run]
#
# Uses the operator paths production uses: VMD registers its host through its
# heartbeat, hostctl activates it, and cmd/seed-templates seeds and builds
# superserve/base under the system team, waiting for the build to finish.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=services/common.sh
. "$here/services/common.sh"

dry_run=false
case "${1:-}" in
  "") ;;
  --dry-run) dry_run=true ;;
  *) die "usage: seed-base.sh [--dry-run]" ;;
esac

load_box_config
load_local_secrets
src=$(cd -- "$here/../.." && pwd)
base_spec="$src/superserve_templates/superserve-base.json"
[[ -r $base_spec ]] || die "missing $base_spec"
host_id=$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["host_id"])' "$DEVBOX_STATE_DIR/runtime.json") ||
  die "runtime.json has no host_id"
bin="$DEVBOX_INSTALL_DIR/api/current"
[[ -x $bin/hostctl && -x $bin/seed-templates ]] || die "API tools are not installed; run install-api.sh first"
export CONTROL_PLANE_URL="http://127.0.0.1:$API_PORT"

host_status() {
  "$bin/hostctl" list | awk -v id="$host_id" 'NR > 1 && $1 == id { print $2 }'
}

if [[ $dry_run == true ]]; then
  log "dry run: host $host_id is '$(host_status)'; would activate it and seed $(basename "$base_spec")"
  exit 0
fi

deadline=$((SECONDS + 300))
while [[ -z $(host_status) ]]; do
  [[ $SECONDS -lt $deadline ]] || die "host $host_id never registered; check VMD's heartbeat to $CONTROL_PLANE_URL"
  sleep 5
done
if [[ $(host_status) != active ]]; then
  "$bin/hostctl" activate "$host_id"
fi
log "host $host_id is $(host_status)"

specs=$(mktemp -d)
trap 'rm -rf "$specs"' EXIT
cp "$base_spec" "$specs/"
DATABASE_URL="postgresql://postgres:$POSTGRES_PASSWORD@127.0.0.1:54322/postgres?sslmode=disable" \
  "$bin/seed-templates" --dir "$specs"
log "base template ready"
