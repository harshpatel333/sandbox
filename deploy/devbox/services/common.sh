# shellcheck shell=bash disable=SC2034
# Shared by services-up.sh, install-api.sh and seed-base.sh. Sourced, not run.

DEVBOX_CONFIG_DIR=${DEVBOX_CONFIG_DIR:-/etc/superserve-devbox}
DEVBOX_STATE_DIR=${DEVBOX_STATE_DIR:-/var/lib/superserve-devbox}
DEVBOX_DATA_DIR=${DEVBOX_DATA_DIR:-/mnt/sandbox-data/superserve-devbox}
DEVBOX_INSTALL_DIR=${DEVBOX_INSTALL_DIR:-/opt/superserve-devbox}

# Fixed by the devbox port contract; everything is reached through IAP SSH
# forwarding of these localhost ports.
CONSOLE_URL=http://localhost:3000
AUTH_URL=http://localhost:54321
API_PORT=8080
EDGE_PROXY_DOMAIN=sandbox.localhost
EMAIL_FROM_ADDRESS=noreply@devbox.localhost

services_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
local_secrets="$DEVBOX_DATA_DIR/local-secrets.env"
promotion_key="$DEVBOX_DATA_DIR/promotion-account-key.pem"
compose_env="$DEVBOX_STATE_DIR/services.env"
stripe_env="$DEVBOX_STATE_DIR/stripe.env"

log() { printf '%s: %s\n' "$(basename -- "$0")" "$*" >&2; }
die() { log "error: $*"; exit 1; }

helper() { python3 -I "$services_dir/devbox_services.py" "$@"; }

compose() {
  docker compose --project-name superserve-devbox --file "$services_dir/compose.yml" \
    --env-file "$compose_env" "$@"
}

# Reads the non-secret settings CI writes (devbox.env) and the CI-delivered
# secrets (secrets.env). Both are shell-quoted KEY=value files.
load_box_config() {
  [[ $EUID -eq 0 ]] || die "run as root"
  local file
  for file in "$DEVBOX_CONFIG_DIR/devbox.env" "$DEVBOX_CONFIG_DIR/secrets.env"; do
    [[ -r $file ]] || die "missing $file"
    set -a
    # shellcheck disable=SC1090
    . "$file"
    set +a
  done
  [[ -n ${DEVBOX_ID:-} ]] || die "DEVBOX_ID is not set in devbox.env"
}

load_local_secrets() {
  [[ -r $local_secrets ]] || die "missing $local_secrets; run services-up.sh first"
  set -a
  # shellcheck disable=SC1090
  . "$local_secrets"
  set +a
}

clear_api_deploy_record() {
  local record="$DEVBOX_STATE_DIR/api-deploy.json" archive="$DEVBOX_STATE_DIR/api-deploy-history" saved
  if [[ -e $record ]]; then
    install -d -m 0700 "$archive"
    saved=$(mktemp -d "$archive/receipt.XXXXXXXX")
    cp -p -- "$record" "$saved/api-deploy.json"
  fi
  rm -f -- "$record"
}
