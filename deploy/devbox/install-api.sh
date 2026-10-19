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
# the east production database carries, because the API's account promotion
# checks read it from this same database. A failing migration stops the
# install before the running API is touched. The outcome is recorded in
# /var/lib/superserve-devbox/api-deploy.json.
#
# --dry-run checks configuration and lists pending migrations without building,
# migrating or restarting anything.
set -euo pipefail

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
sha=$(git -C "$src" rev-parse HEAD)
runtime_json="$DEVBOX_STATE_DIR/runtime.json"
[[ -r $runtime_json ]] || die "missing $runtime_json; run host-bootstrap.sh first"
deploy_record="$DEVBOX_STATE_DIR/api-deploy.json"
release="$DEVBOX_INSTALL_DIR/api/releases/$sha"

psql_admin() {
  compose exec -T db psql --no-psqlrc -v ON_ERROR_STOP=1 -q -At -U supabase_admin -d postgres -p 54322 "$@"
}

applied_versions() {
  psql_admin -c "SELECT version FROM supabase_migrations.schema_migrations ORDER BY version" 2>/dev/null || true
}

record() {
  local status=$1; shift
  python3 -I -c 'import json, sys; print(json.dumps({"sha": sys.argv[1], "migrations": sys.argv[2], "applied": sys.argv[3:]}))' \
    "$sha" "$status" "$@" > "$deploy_record.tmp"
  mv "$deploy_record.tmp" "$deploy_record"
}

go_version() { sed -n 's/^go \([0-9.]*\)$/\1/p' "$src/go.mod"; }

ensure_go() {
  local version root
  version=$(go_version)
  [[ -n $version ]] || die "cannot read the Go version from go.mod"
  root="$DEVBOX_INSTALL_DIR/tools/go$version"
  if [[ ! -x $root/bin/go ]]; then
    local tarball="go$version.linux-amd64.tar.gz" want tmp
    want=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
      python3 -I -c 'import json, sys
name = sys.argv[1]
print(next(f["sha256"] for r in json.load(sys.stdin) for f in r["files"] if f["filename"] == name))' "$tarball") ||
      die "no published checksum for $tarball"
    tmp=$(mktemp -d)
    curl -fsSL -o "$tmp/$tarball" "https://go.dev/dl/$tarball"
    echo "$want  $tmp/$tarball" | sha256sum --check --quiet - || die "checksum mismatch for $tarball"
    mkdir -p "$root"
    tar -xzf "$tmp/$tarball" -C "$root" --strip-components=1
    rm -rf "$tmp"
  fi
  export PATH="$root/bin:$PATH" GOTOOLCHAIN=local GOFLAGS=-mod=readonly
}

ensure_supabase_cli() {
  local dir="$DEVBOX_INSTALL_DIR/tools/supabase-$SUPABASE_CLI_VERSION"
  supabase="$dir/supabase"
  [[ -x $supabase ]] && return
  local tmp
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/cli.tar.gz" \
    "https://github.com/supabase/cli/releases/download/v$SUPABASE_CLI_VERSION/supabase_linux_amd64.tar.gz"
  echo "$SUPABASE_CLI_SHA256  $tmp/cli.tar.gz" | sha256sum --check --quiet - || die "Supabase CLI checksum mismatch"
  mkdir -p "$dir"
  tar -xzf "$tmp/cli.tar.gz" -C "$dir" supabase
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
  [[ -n $(go_version) ]] || die "cannot read the Go version from go.mod"
  tmp_env=$(mktemp)
  trap 'rm -f "$tmp_env"' EXIT
  render_api_env "$tmp_env"
  log "dry run: API configuration is valid for $sha"
  log "pending migrations: $(pending_migrations | tr '\n' ' ')"
  exit 0
fi

ensure_go
ensure_supabase_cli

log "building control plane at $sha"
mkdir -p "$release"
(cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$release/" ./cmd/controlplane ./cmd/seed-templates ./cmd/hostctl)

mapfile -t pending < <(pending_migrations)
workdir=$(migration_workdir)
log "applying ${#pending[@]} pending migration(s)"
if ! (cd "$workdir" && SUPABASE_TELEMETRY_DISABLED=1 "$supabase" db push --yes \
  --db-url "postgresql://postgres:$POSTGRES_PASSWORD@127.0.0.1:54322/postgres?sslmode=disable"); then
  rm -rf "$workdir"
  mapfile -t done_versions < <(comm -23 <(printf '%s\n' "${pending[@]}") <(pending_migrations))
  record failed "${done_versions[@]}"
  die "migrations failed; the running API was not changed"
fi
rm -rf "$workdir"
if [[ ${#pending[@]} -gt 0 ]]; then
  record applied "${pending[@]}"
else
  record none
fi

# The API reads promotion evidence through this RPC-only role, as production
# does. The system team owns the curated templates; the API lifts its quotas
# at startup, so it must exist before the API starts.
psql_admin <<SQL
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
