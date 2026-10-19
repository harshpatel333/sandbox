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
  devbox_load_local_secrets INTERNAL_API_TOKEN DATABASE_URL
  local revision bin
  revision=$(devbox_revision)
  bin=$DEVBOX_REPO/bin
  mkdir -p "$bin"
  for binary in vmd boxd template-builder secretsproxy; do
    devbox_log "building $binary at ${revision:0:12}"
    devbox_build "$bin/$binary" "./cmd/$binary"
  done
  # Fleet deploys stage everything under this directory; run from the
  # checkout so the bundle paths resolve the same way.
  cd "$DEVBOX_REPO"
  devbox_log "installing VMD on $DEVBOX_INSTANCE"
  DEPLOY_TRANSPORT=local DEPLOY_INSTANCE=$DEVBOX_INSTANCE DEPLOY_ZONE=$DEVBOX_ZONE \
    SHA=$revision VMD_SERVICE=superserve-vmd VMD_INSTALL_DIR=/usr/local/bin \
    CONTROL_PLANE_URL=$DEVBOX_API_URL BACKUP_BUCKET=$DEVBOX_BUCKET \
    python3 .github/workflows/scripts/deploy-vmd.py \
    || devbox_die "VMD install failed; see journalctl -u superserve-vmd -u superserve-secretsproxy"
  devbox_log "VMD ready at ${revision:0:12}"
}

main "$@"
