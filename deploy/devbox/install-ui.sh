#!/usr/bin/env bash
# Build the console from the checked-out UI revision and (re)start it on
# 127.0.0.1:3000 as the superserve-devbox-console systemd service.
#
# Runs as root on a devbox after services-up.sh and install-api.sh; safe to
# re-run (create and upgrade both call it). A failed build leaves the running
# console untouched.
#
# Inputs:
#   /etc/superserve-devbox/devbox.env    non-secret settings from CI
#                                        (RECAPTCHA_SITE_KEY, optional)
#   /etc/superserve-devbox/secrets.env   CI secrets (RECAPTCHA_API_KEY,
#                                        RECAPTCHA_PROJECT_ID, optional)
#   $SHARED_SECRETS_FILE                 secrets services-up.sh generates for
#                                        the console and the API to share
#   /opt/superserve-devbox/src/superserve  UI checkout
#
# Public values (NEXT_PUBLIC_*) are compiled into the browser bundle at build
# time; everything else is only given to the server at runtime. The browser
# reaches the box through SSH port forwarding on the same ports; see the
# console README ("Private single-box environments").
#
# Usage: install-ui.sh [--dry-run]
#   --dry-run  print the build arguments, runtime variable names and unit
#              without building, writing or restarting anything.
set -euo pipefail

CONFIG_DIR="${DEVBOX_CONFIG_DIR:-/etc/superserve-devbox}"
UI_SRC="${DEVBOX_UI_SRC:-/opt/superserve-devbox/src/superserve}"
SECRETS_DIR="${DEVBOX_SECRETS_DIR:-/mnt/sandbox-data/superserve-devbox/secrets}"
SHARED_SECRETS_FILE="${DEVBOX_SHARED_SECRETS_FILE:-$SECRETS_DIR/console.env}"
CONSOLE_SECRETS_FILE="$SECRETS_DIR/console-local.env"
RUNTIME_DIR="${DEVBOX_CONSOLE_RUNTIME_DIR:-/var/lib/superserve-devbox/console}"
UNIT_FILE="${DEVBOX_CONSOLE_UNIT_FILE:-/etc/systemd/system/superserve-devbox-console.service}"
READY_TIMEOUT="${DEVBOX_CONSOLE_READY_TIMEOUT:-180}"

SERVICE=superserve-devbox-console
IMAGE=superserve-devbox-console:current
CONSOLE_URL=http://localhost:3000
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

DRY_RUN=0
case "${1:-}" in
  "") ;;
  --dry-run) DRY_RUN=1 ;;
  *) echo "usage: $0 [--dry-run]" >&2; exit 2 ;;
esac

die() {
  echo "install-ui: $*" >&2
  exit 1
}

log() {
  echo "install-ui: $*"
}

load_env() {
  [[ -r "$1" ]] || die "$1 is missing or unreadable${2:+ ($2)}"
  set -a
  # shellcheck disable=SC1090
  . "$1"
  set +a
}

require() {
  local name
  for name in "$@"; do
    [[ -n "${!name:-}" ]] || die "$name is not set in $SHARED_SECRETS_FILE (re-run services-up.sh)"
  done
}

ensure_console_secrets() {
  if [[ ! -s "$CONSOLE_SECRETS_FILE" ]]; then
    log "generating console secrets in $CONSOLE_SECRETS_FILE"
    install -d -m 0700 "$SECRETS_DIR"
    (
      umask 077
      printf 'CONSOLE_PROXY_SECRET=%q\nGOOGLE_SIGNUP_PROOF_SECRET=%q\n' \
        "$(openssl rand -base64 48 | tr -d '\n')" \
        "$(openssl rand -base64 48 | tr -d '\n')" >"$CONSOLE_SECRETS_FILE.tmp"
    )
    mv "$CONSOLE_SECRETS_FILE.tmp" "$CONSOLE_SECRETS_FILE"
  fi
  load_env "$CONSOLE_SECRETS_FILE"
}

load_env "$CONFIG_DIR/devbox.env" "written by the create workflow"
load_env "$CONFIG_DIR/secrets.env" "written by the create workflow"
load_env "$SHARED_SECRETS_FILE" "run services-up.sh first"
require NEXT_PUBLIC_SUPABASE_ANON_KEY SUPABASE_SERVICE_ROLE_KEY \
  INTERNAL_API_TOKEN SANDBOX_INTERNAL_API_TOKEN \
  PROMOTION_CAPTURE_TOKEN PROMOTION_ACCOUNT_TOKEN PROMOTION_ACCOUNT_PRIVATE_KEY \
  YC_CLAIM_CONSOLE_TOKEN
[[ -f "$UI_SRC/apps/console/package.json" ]] || die "$UI_SRC is not a superserve checkout"

BUILD_ARGS=(
  "NEXT_PUBLIC_APP_URL=$CONSOLE_URL"
  "NEXT_PUBLIC_WEBSITE_URL=https://www.superserve.ai"
  "NEXT_PUBLIC_SUPABASE_URL=http://localhost:54321"
  "NEXT_PUBLIC_SUPABASE_ANON_KEY=$NEXT_PUBLIC_SUPABASE_ANON_KEY"
  "NEXT_PUBLIC_SANDBOX_SCHEME=http"
  "NEXT_PUBLIC_SANDBOX_HOST=sandbox.localhost"
  "NEXT_PUBLIC_SANDBOX_PORT=8090"
  "NEXT_PUBLIC_RECAPTCHA_SITE_KEY=${RECAPTCHA_SITE_KEY:-}"
)

RUNTIME_VARS=(
  SANDBOX_API_URL=http://localhost:8080
  EMAIL_TRANSPORT=smtp
  SMTP_HOST=127.0.0.1
  SMTP_PORT=1025
  "RESEND_FROM_ADDRESS=Superserve <team@superserve.localhost>"
  "SUPABASE_SERVICE_ROLE_KEY=$SUPABASE_SERVICE_ROLE_KEY"
  "INTERNAL_API_TOKEN=$INTERNAL_API_TOKEN"
  "SANDBOX_INTERNAL_API_TOKEN=$SANDBOX_INTERNAL_API_TOKEN"
  "PROMOTION_CAPTURE_TOKEN=$PROMOTION_CAPTURE_TOKEN"
  "PROMOTION_ACCOUNT_TOKEN=$PROMOTION_ACCOUNT_TOKEN"
  "PROMOTION_ACCOUNT_PRIVATE_KEY=$PROMOTION_ACCOUNT_PRIVATE_KEY"
  "YC_CLAIM_CONSOLE_TOKEN=$YC_CLAIM_CONSOLE_TOKEN"
  "RECAPTCHA_API_KEY=${RECAPTCHA_API_KEY:-}"
  "RECAPTCHA_PROJECT_ID=${RECAPTCHA_PROJECT_ID:-}"
)

DOCKER="$(command -v docker || echo /usr/bin/docker)"
UNIT="[Unit]
Description=Superserve devbox console
Requires=docker.service
After=docker.service

[Service]
ExecStartPre=-$DOCKER rm -f $SERVICE
ExecStart=$DOCKER run --rm --name $SERVICE --network host -v $RUNTIME_DIR/runtime.env:/run/superserve/console.env:ro $IMAGE
ExecStop=$DOCKER stop $SERVICE
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
"

if ((DRY_RUN)); then
  printf 'build-arg %s\n' "${BUILD_ARGS[@]}"
  printf 'runtime %s=…\n' "${RUNTIME_VARS[@]%%=*}" CONSOLE_PROXY_SECRET GOOGLE_SIGNUP_PROOF_SECRET
  printf '%s' "$UNIT"
  exit 0
fi

[[ $EUID -eq 0 ]] || die "must run as root"
command -v docker >/dev/null || die "docker is not installed"

ensure_console_secrets
RUNTIME_VARS+=(
  "CONSOLE_PROXY_SECRET=$CONSOLE_PROXY_SECRET"
  "GOOGLE_SIGNUP_PROOF_SECRET=$GOOGLE_SIGNUP_PROOF_SECRET"
)

revision="$(git -C "$UI_SRC" rev-parse HEAD 2>/dev/null || echo unknown)"
log "building console at $revision"
docker_args=()
for arg in "${BUILD_ARGS[@]}"; do
  docker_args+=(--build-arg "$arg")
done
DOCKER_BUILDKIT=1 docker build --progress=plain \
  -f "$SCRIPT_DIR/console.Dockerfile" \
  --build-arg "UI_REVISION=$revision" "${docker_args[@]}" \
  -t "$IMAGE" "$UI_SRC" || die "console build failed; the previous console is still running"

install -d -m 0700 "$RUNTIME_DIR"
(
  umask 077
  for var in "${RUNTIME_VARS[@]}"; do
    printf '%s=%q\n' "${var%%=*}" "${var#*=}"
  done >"$RUNTIME_DIR/runtime.env.tmp"
)
mv "$RUNTIME_DIR/runtime.env.tmp" "$RUNTIME_DIR/runtime.env"

printf '%s' "$UNIT" >"$UNIT_FILE"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
log "restarting $SERVICE"
systemctl restart "$SERVICE"

deadline=$((SECONDS + READY_TIMEOUT))
until status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:3000/)" &&
  [[ "$status" =~ ^[23] ]]; do
  if ((SECONDS >= deadline)); then
    journalctl -u "$SERVICE" -n 50 --no-pager >&2 || true
    die "console did not become ready on 127.0.0.1:3000 within ${READY_TIMEOUT}s"
  fi
  sleep 2
done

docker image prune -f --filter label=superserve-devbox=console >/dev/null || true
log "console $revision ready at $CONSOLE_URL"
