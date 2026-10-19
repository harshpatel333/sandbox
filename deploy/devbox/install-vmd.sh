#!/bin/bash
# Build VMD and its host binaries from the checked-out sandbox revision and
# install them with the fleet's own deploy script, run against this host. The
# same path serves create and upgrade: binaries, units and guards are swapped
# while the socket holds connections, paused and running sandboxes are kept,
# and the script returns only once VMD serves requests on the new binary.
set -euo pipefail

# shellcheck source=deploy/devbox/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

main() {
  devbox_require_root
  devbox_load_env
  devbox_require_bootstrapped
  devbox_instance
  devbox_load_local_secrets INTERNAL_API_TOKEN
  if [ -z "${DATABASE_URL:-}" ]; then
    [ -n "${POSTGRES_PASSWORD:-}" ] || devbox_die "local-secrets.env has neither DATABASE_URL nor POSTGRES_PASSWORD"
    DATABASE_URL=$(python3 -c 'import sys, urllib.parse; print("postgresql://postgres:" + urllib.parse.quote(sys.argv[1], safe="") + "@127.0.0.1:54322/postgres?sslmode=disable")' "$POSTGRES_PASSWORD")
  fi
  local revision bin
  revision=$(devbox_revision)
  bin=$DEVBOX_REPO/bin
  mkdir -p "$bin"
  for binary in vmd boxd template-builder secretsproxy; do
    devbox_log "building $binary at ${revision:0:12}"
    devbox_build "$bin/$binary" "./cmd/$binary"
  done
  devbox_log "installing VMD on $DEVBOX_INSTANCE"
  local status=0
  devbox_deploy SHA="$revision" CONTROL_PLANE_URL="$DEVBOX_API_URL" BACKUP_BUCKET="$DEVBOX_BUCKET" \
    DATABASE_URL="$DATABASE_URL" INTERNAL_API_TOKEN="$INTERNAL_API_TOKEN" \
    python3 .github/workflows/scripts/deploy-vmd.py || status=$?
  rm -f "$DEVBOX_REPO"/deploy-bundle-*.tar.gz
  [ "$status" = 0 ] || devbox_die "VMD install failed; see journalctl -u superserve-vmd -u superserve-secretsproxy"
  devbox_log "VMD ready at ${revision:0:12}"
}

main "$@"
