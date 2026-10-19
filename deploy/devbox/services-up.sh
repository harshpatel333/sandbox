#!/usr/bin/env bash
# Start or update this devbox's local services: Postgres, Auth, the data API,
# the auth/data gateway, Mailpit and (when a Stripe key is configured) Stripe
# event forwarding. Generates local secrets on first run and writes
# /var/lib/superserve-devbox/services.json. Safe to re-run; data persists on the
# data disk and destroying the box is the only reset.
#
#   services-up.sh [--dry-run]
#
# --dry-run validates configuration and prints the compose plan without
# installing packages or starting containers.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=services/common.sh
. "$here/services/common.sh"

dry_run=false
case "${1:-}" in
  "") ;;
  --dry-run) dry_run=true ;;
  *) die "usage: services-up.sh [--dry-run]" ;;
esac

load_box_config

# Docker must never manage iptables here: its default FORWARD policy and
# bridge rules would break VMD's sandbox forwarding. The daemon config is
# written before the package is installed because installation starts the
# daemon immediately.
ensure_docker() {
  if [[ $dry_run == true ]]; then
    log "would ensure Docker with iptables management disabled"
    return
  fi
  mkdir -p /etc/docker
  local changed=0
  helper docker-daemon-config /etc/docker/daemon.json || changed=$?
  [[ $changed -le 1 ]] || die "could not write /etc/docker/daemon.json"
  if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
    log "installing Docker"
    apt-get update -q
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q docker.io docker-compose-v2
  elif [[ $changed -eq 1 ]]; then
    log "restarting Docker to apply daemon configuration"
    systemctl restart docker
  fi
  systemctl enable --now docker >/dev/null
}

render_compose_env() {
  helper compose-env "$local_secrets" "$compose_env" \
    "DEVBOX_DATA_DIR=$DEVBOX_DATA_DIR" \
    "AUTH_EXTERNAL_URL=$AUTH_URL/auth/v1" \
    "CONSOLE_URL=$CONSOLE_URL" \
    "EMAIL_FROM_ADDRESS=$EMAIL_FROM_ADDRESS" \
    "API_PORT=$API_PORT" \
    "STRIPE_SECRET_KEY=${STRIPE_SECRET_KEY:-}" \
    "STRIPE_DEVICE_NAME=superserve-devbox-$DEVBOX_ID"
}

wait_http() {
  local name=$1 url=$2; shift 2
  local deadline=$((SECONDS + 120))
  until curl -fs -o /dev/null --max-time 5 "$@" "$url"; do
    [[ $SECONDS -lt $deadline ]] || die "$name did not become ready at $url"
    sleep 2
  done
  log "$name ready"
}

# The listener prints its signing secret when the session starts. Reading it
# from the running session keeps the API's secret in step with the listener.
capture_stripe_secret() {
  local deadline=$((SECONDS + 120)) secret=""
  while [[ -z $secret ]]; do
    secret=$(compose logs --no-log-prefix stripe 2>/dev/null |
      sed -n 's/.*webhook signing secret is \(whsec_[A-Za-z0-9]*\).*/\1/p' | tail -n 1)
    [[ -n $secret || $SECONDS -lt $deadline ]] || die "stripe listen did not report a signing secret; check: docker compose -p superserve-devbox logs stripe"
    [[ -n $secret ]] || sleep 2
  done
  helper write-env "$stripe_env" "STRIPE_WEBHOOK_SECRET=$secret" "STRIPE_METER_ERROR_WEBHOOK_SECRET=$secret"
  log "stripe listen forwarding to 127.0.0.1:$API_PORT/stripe/webhook"
}

ensure_docker
if [[ $dry_run == true ]]; then
  scratch=$(mktemp -d)
  trap 'rm -rf "$scratch"' EXIT
  [[ -r $local_secrets ]] && cp "$local_secrets" "$scratch/local.env"
  local_secrets=$scratch/local.env promotion_key=$scratch/key.pem compose_env=$scratch/services.env
else
  install -d -m 0755 "$DEVBOX_DATA_DIR" "$DEVBOX_STATE_DIR"
  install -d -m 0700 "$DEVBOX_DATA_DIR/secrets"
fi
helper secrets "$local_secrets" "$promotion_key"
render_compose_env

profiles=()
if [[ -n ${STRIPE_SECRET_KEY:-} ]]; then
  profiles=(--profile stripe)
else
  log "STRIPE_SECRET_KEY is not set in secrets.env; Stripe event forwarding stays off"
fi

if [[ $dry_run == true ]]; then
  compose "${profiles[@]}" config --quiet
  compose "${profiles[@]}" config --images
  log "dry run: configuration is valid"
  exit 0
fi

install -d -m 0700 "$DEVBOX_DATA_DIR/postgres"
install -d -m 0755 "$DEVBOX_DATA_DIR/mailpit"
compose "${profiles[@]}" up --detach --wait --remove-orphans

load_local_secrets
wait_http "Auth" "http://127.0.0.1:54321/auth/v1/health" -H "apikey: $ANON_KEY"
wait_http "data API" "http://127.0.0.1:54321/rest/v1/" -H "apikey: $ANON_KEY" -H "Authorization: Bearer $ANON_KEY"
wait_http "Mailpit" "http://127.0.0.1:8025/livez"
if [[ ${#profiles[@]} -gt 0 ]]; then
  capture_stripe_secret
else
  rm -f "$stripe_env"
  compose rm --stop --force stripe >/dev/null 2>&1 || true
fi

helper services-json "$DEVBOX_STATE_DIR/services.json" "$here/services/compose.yml" "${STRIPE_SECRET_KEY:+stripe}"
log "services ready; wrote $DEVBOX_STATE_DIR/services.json"
