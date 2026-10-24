#!/usr/bin/env bash
# Build the control plane from this checkout, apply pending database
# migrations forward, then (re)start the API on 127.0.0.1:8080 and wait until
# it is ready. The API runs its background jobs in-process. Used by both
# create and deploy; run services-up.sh first.
#
#   install-api.sh [--dry-run]
#
# Migrations go through the same pinned Supabase CLI and `db push` history as
# the staging and production deployments, plus the shared-Auth history entry
# of the production database that also hosts Auth, because the API's account
# promotion checks read it from this same database. A failing migration stops the
# install before the running API is touched. The outcome is recorded in
# /var/lib/superserve-devbox/api-deploy.json.
#
# --dry-run checks configuration and lists pending migrations without building,
# migrating or restarting anything.
set -euo pipefail
shopt -s inherit_errexit

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=services/common.sh
. "$here/services/common.sh"

SUPABASE_CLI_VERSION=2.119.0
SUPABASE_CLI_SHA256=bf1c3ae93be98533eb8a3105dbf4564bd0b2d9dc24690d8a920f980ef975c1b4

dry_run=false
case "${1:-}" in
  "") ;;
  --dry-run) dry_run=true ;;
  *) die "usage: install-api.sh [--dry-run]" ;;
esac

load_box_config
load_local_secrets
src=$(cd -- "$here/../.." && pwd)
sha=$(git -C "$src" rev-parse HEAD 2>/dev/null) || die "$src is not a git checkout"
runtime_json="$DEVBOX_STATE_DIR/runtime.json"
[[ -r $runtime_json ]] || die "missing $runtime_json; run host-bootstrap.sh first"
deploy_record="$DEVBOX_STATE_DIR/api-deploy.json"
release="$DEVBOX_INSTALL_DIR/api/releases/$sha"

psql_admin() {
  compose exec -T db psql --no-psqlrc -v ON_ERROR_STOP=1 -q -At -U supabase_admin -d postgres -p 54322 "$@"
}

# The history table appears with the first migration; until then nothing is
# applied. Any other query failure must stop the install.
applied_versions() {
  local table
  table=$(psql_admin -c "SELECT to_regclass('supabase_migrations.schema_migrations')") || die "cannot query the database"
  [[ -n $table ]] || return 0
  psql_admin -c "SELECT version FROM supabase_migrations.schema_migrations ORDER BY version" || die "cannot read migration history"
}

record() {
  local status=$1; shift
  python3 -I -c 'import json, sys; print(json.dumps({"sha": sys.argv[1], "migrations": sys.argv[2], "applied": sys.argv[3:]}))' \
    "$sha" "$status" "$@" > "$deploy_record.tmp"
  mv "$deploy_record.tmp" "$deploy_record"
}

# host-bootstrap.sh installs Go; the go command fetches a newer toolchain
# itself when go.mod asks for one.
ensure_go() {
  [[ -x /usr/local/go/bin/go ]] || die "missing /usr/local/go/bin/go; run host-bootstrap.sh first"
  export PATH="/usr/local/go/bin:$PATH" GOFLAGS=-mod=readonly
}

ensure_supabase_cli() {
  local dir="$DEVBOX_INSTALL_DIR/tools/supabase-$SUPABASE_CLI_VERSION"
  supabase="$dir/supabase"
  [[ -x $supabase ]] && return
  local tmp
  tmp=$(mktemp -d "$DEVBOX_INSTALL_DIR/tools/.supabase.XXXXXX")
  curl -fsSL -o "$tmp/cli.tar.gz" \
    "https://github.com/supabase/cli/releases/download/v$SUPABASE_CLI_VERSION/supabase_linux_amd64.tar.gz"
  echo "$SUPABASE_CLI_SHA256  $tmp/cli.tar.gz" | sha256sum --check --quiet - || die "Supabase CLI checksum mismatch"
  tar -xzf "$tmp/cli.tar.gz" -C "$tmp" supabase
  mkdir -p "$dir"
  mv "$tmp/supabase" "$supabase"
  rm -rf "$tmp"
}

# The CLI applies every migration file whose version is not yet in
# supabase_migrations.schema_migrations, in version order.
migration_workdir() {
  local dir
  dir=$(mktemp -d)
  mkdir -p "$dir/supabase/migrations"
  printf 'project_id = "superserve-devbox"\n' > "$dir/supabase/config.toml"
  cp "$src"/supabase/migrations/*.sql "$src"/supabase/shared-auth-history/*.sql "$dir/supabase/migrations/"
  echo "$dir"
}

pending_migrations() {
  local applied
  applied=$(applied_versions)
  local file version
  for file in "$src"/supabase/migrations/*.sql "$src"/supabase/shared-auth-history/*.sql; do
    version=$(basename "$file"); version=${version%%_*}
    grep -qx "$version" <<<"$applied" || echo "$version"
  done | sort
}

render_api_env() {
  if [[ -r $stripe_env ]]; then
    set -a
    # shellcheck disable=SC1090
    . "$stripe_env"
    set +a
  fi
  EMAIL_FROM="Superserve <$EMAIL_FROM_ADDRESS>" CONSOLE_URL=$CONSOLE_URL EDGE_PROXY_DOMAIN=$EDGE_PROXY_DOMAIN \
    API_PORT=$API_PORT SANDBOX_SHA=$sha helper api-env "$1" "$runtime_json"
}

if [[ $dry_run == true ]]; then
  tmp_env=$(mktemp)
  trap 'rm -f "$tmp_env"' EXIT
  render_api_env "$tmp_env"
  log "dry run: API configuration is valid for $sha"
  pending_list=$(pending_migrations)
  log "pending migrations: ${pending_list//$'\n'/ }"
  exit 0
fi

[[ $PROMOTION_EVIDENCE_PROXY_PASSWORD =~ ^[0-9a-f]+$ ]] || die "PROMOTION_EVIDENCE_PROXY_PASSWORD in $local_secrets must be hex"
[[ $SYSTEM_TEAM_ID =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || die "SYSTEM_TEAM_ID in $local_secrets must be a UUID"
rm -f "$deploy_record"
mkdir -p "$DEVBOX_INSTALL_DIR/tools"
ensure_go
ensure_supabase_cli

log "building control plane at $sha"
mkdir -p "$release"
(cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$release/" ./cmd/controlplane ./cmd/seed-templates ./cmd/hostctl ./cmd/pricing-v2-migrate)

"$here/pricing-integration-reset.sh"
export TEAM_CREATION_REGION=use

pending_list=$(pending_migrations)
mapfile -t pending <<<"$pending_list"
[[ -n ${pending[0]} ]] || pending=()
workdir=$(migration_workdir)

record_partial_failure() {
  local still done_versions
  still=$(pending_migrations) || still=$pending_list
  mapfile -t done_versions < <(comm -23 <(printf '%s\n' "${pending[@]}") <(printf '%s\n' "$still"))
  record failed "${done_versions[@]}"
}

# A run cancelled or killed while migrating still reports failed, with the
# versions that committed before it stopped.
migrating=true
trap 'if [[ $migrating == true ]]; then migrating=false; record_partial_failure || true; fi; rm -rf "$workdir"' EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 141' PIPE
log "applying ${#pending[@]} pending migration(s)"
if ! (cd "$workdir" && PGPASSWORD=$POSTGRES_PASSWORD SUPABASE_TELEMETRY_DISABLED=1 "$supabase" db push --yes \
  --db-url "postgresql://postgres@127.0.0.1:54322/postgres?sslmode=disable"); then
  migrating=false
  record_partial_failure
  die "migrations failed; the running API was not changed"
fi
migrating=false
if [[ ${#pending[@]} -gt 0 ]]; then
  record applied "${pending[@]}"
else
  record none
fi

# The API reads promotion evidence through this RPC-only role, as production
# does. The system team owns the curated templates; the API lifts its quotas
# at startup, so it must exist before the API starts. The session never logs
# the statement, which carries a password.
psql_admin <<SQL
SET log_statement = 'none';
SET log_min_error_statement = 'panic';
SET pgaudit.log = 'none';
SET log_min_duration_statement = -1;
ALTER ROLE promotion_evidence_proxy PASSWORD '$PROMOTION_EVIDENCE_PROXY_PASSWORD';
INSERT INTO team (id, name) VALUES ('$SYSTEM_TEAM_ID', 'superserve-system') ON CONFLICT (id) DO NOTHING;
SQL

render_api_env "$DEVBOX_STATE_DIR/api.env"
ln -sfn "$release" "$DEVBOX_INSTALL_DIR/api/current"
sed -e "s|@INSTALL_DIR@|$DEVBOX_INSTALL_DIR|g" -e "s|@STATE_DIR@|$DEVBOX_STATE_DIR|g" \
  "$services_dir/superserve-devbox-api.service" > /etc/systemd/system/superserve-devbox-api.service
systemctl daemon-reload
systemctl enable superserve-devbox-api.service >/dev/null
systemctl restart superserve-devbox-api.service

deadline=$((SECONDS + 120))
until curl -fs -o /dev/null --max-time 5 "http://127.0.0.1:$API_PORT/health"; do
  if [[ $SECONDS -ge $deadline ]] || ! systemctl is-active --quiet superserve-devbox-api.service; then
    journalctl -u superserve-devbox-api.service -n 50 --no-pager >&2 || true
    die "API did not become ready on 127.0.0.1:$API_PORT"
  fi
  sleep 2
done
log "API ready at $sha"

systemctl start superserve-vmd
"$here/seed-base.sh"
