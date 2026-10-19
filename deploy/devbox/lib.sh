# shellcheck shell=bash
# Shared by the devbox entry points; sourced, never run.
#
# Every entry point runs as root on the box from the checked-out sandbox
# revision, reads the CI-written settings and secrets, and exits non-zero with
# a message naming what is wrong.

DEVBOX_ETC=${DEVBOX_ETC:-/etc/superserve-devbox}
DEVBOX_STATE=${DEVBOX_STATE:-/var/lib/superserve-devbox}
DEVBOX_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
DEVBOX_RUNTIME_JSON=$DEVBOX_STATE/runtime.json
DEVBOX_GO=/usr/local/go/bin/go

devbox_log() {
  printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2
}

devbox_die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

devbox_require_root() {
  [ "$(id -u)" -eq 0 ] || devbox_die "run as root"
}

# Pins first so devbox.env can override any of them; secrets.env is optional
# until a step needs one of its values.
devbox_load_env() {
  [ -r "$DEVBOX_ETC/devbox.env" ] || devbox_die "$DEVBOX_ETC/devbox.env is missing; the create workflow writes it"
  set -a
  # shellcheck source=deploy/devbox/runtime-pins.env
  . "$DEVBOX_REPO/deploy/devbox/runtime-pins.env"
  # shellcheck disable=SC1091
  . "$DEVBOX_ETC/devbox.env"
  if [ -r "$DEVBOX_ETC/secrets.env" ]; then
    # shellcheck disable=SC1091
    . "$DEVBOX_ETC/secrets.env"
  fi
  set +a
  [[ "${DEVBOX_NAME:-}" =~ ^[a-z][a-z0-9-]{0,38}[a-z0-9]$ ]] || devbox_die "DEVBOX_NAME is missing or invalid"
  [[ "${DEVBOX_ID:-}" =~ ^[0-9a-f]{12}$ ]] || devbox_die "DEVBOX_ID is missing or invalid"
  export DEVBOX_BUCKET=${DEVBOX_BUCKET:-superserve-devbox-$DEVBOX_ID}
  export DEVBOX_ARTIFACT_BUCKET=${DEVBOX_ARTIFACT_BUCKET:-rayai-dev-devbox-hostprep}
  export DEVBOX_DATA_DISK_DEVICE=${DEVBOX_DATA_DISK_DEVICE:-devbox-data}
  export DEVBOX_PROXY_ADDR=${DEVBOX_PROXY_ADDR:-127.0.0.1:8090}
  if ! [[ "$DEVBOX_PROXY_ADDR" =~ ^127\.0\.0\.1:([1-9][0-9]{0,4})$ ]] || [ "${BASH_REMATCH[1]}" -gt 65535 ]; then
    devbox_die "DEVBOX_PROXY_ADDR must be a 127.0.0.1 address"
  fi
  export DEVBOX_API_URL=${DEVBOX_API_URL:-http://127.0.0.1:8080}
  export DEVBOX_CONSOLE_ORIGIN=${DEVBOX_CONSOLE_ORIGIN:-http://localhost:3000}
}

devbox_metadata() {
  curl -sf --max-time 5 -H 'Metadata-Flavor: Google' \
    "http://metadata.google.internal/computeMetadata/v1/$1" \
    || devbox_die "GCE metadata $1 is unavailable; devboxes run on GCE"
}

# shellcheck disable=SC2034
devbox_instance() {
  DEVBOX_INSTANCE=$(devbox_metadata instance/name)
  DEVBOX_ZONE=$(devbox_metadata instance/zone)
  DEVBOX_ZONE=${DEVBOX_ZONE##*/}
  DEVBOX_REGION=${DEVBOX_ZONE%-*}
}

# Values generated on the box on first boot and shared between the host
# runtime and the local services; the services step creates them.
devbox_load_local_secrets() {
  local file=$DEVBOX_ETC/local-secrets.env
  [ -r "$file" ] || devbox_die "$file is missing; run services-up.sh first"
  set -a
  # shellcheck disable=SC1090
  . "$file"
  set +a
  local key
  for key in "$@"; do
    [ -n "${!key:-}" ] || devbox_die "$key is missing from $file"
  done
}

# The checkout's revision, which must be the one CI selected when it says.
# Builds compile the working tree, so it must be exactly that revision.
devbox_revision() {
  local head selected dirty
  head=$(git -c safe.directory="$DEVBOX_REPO" -C "$DEVBOX_REPO" rev-parse HEAD) || devbox_die "cannot read the sandbox revision at $DEVBOX_REPO"
  dirty=$(git -c safe.directory="$DEVBOX_REPO" -C "$DEVBOX_REPO" status --porcelain --untracked-files=all) \
    || devbox_die "cannot read the checkout status at $DEVBOX_REPO"
  [ -z "$dirty" ] || devbox_die "$DEVBOX_REPO has changes outside revision $head; push them to a branch and deploy that branch"
  if [ -n "${DEVBOX_SANDBOX_SHA:-}" ]; then
    [[ "$DEVBOX_SANDBOX_SHA" =~ ^[0-9a-fA-F]{7,40}$ ]] || devbox_die "DEVBOX_SANDBOX_SHA must be a commit SHA"
    selected=$(git -c safe.directory="$DEVBOX_REPO" -C "$DEVBOX_REPO" rev-parse --verify --quiet "$DEVBOX_SANDBOX_SHA^{commit}") || selected=$DEVBOX_SANDBOX_SHA
    [ "$head" = "$selected" ] || devbox_die "$DEVBOX_REPO is at $head, not the selected $DEVBOX_SANDBOX_SHA"
  fi
  printf '%s\n' "$head"
}

devbox_require_bootstrapped() {
  if [ ! -s "$DEVBOX_RUNTIME_JSON" ] || [ ! -e /var/lib/sandbox/.host-bootstrapped ]; then
    devbox_die "the host is not bootstrapped; run host-bootstrap.sh first"
  fi
  # Upgrades keep the runtime the box was created with; a revision that
  # moves the pins is tested against the old ones until the box is recreated.
  python3 - "$DEVBOX_RUNTIME_JSON" <<'PY' || true
import json, os, sys
artifacts = json.load(open(sys.argv[1])).get("artifacts", {})
for name, key, field, pin in (
    ("Firecracker", "firecracker", "sha256", "DEVBOX_FIRECRACKER_SHA256"),
    ("kernel", "kernel", "digest", "DEVBOX_KERNEL_DIGEST"),
    ("base rootfs", "rootfs", "digest", "DEVBOX_ROOTFS_DIGEST"),
):
    installed, pinned = artifacts.get(key, {}).get(field, ""), os.environ.get(pin, "")
    if installed != pinned:
        print(f"WARNING: this box runs the {name} it was created with ({installed[:12]}), "
              f"not this revision's pin ({pinned[:12]}); recreate the box to test the new one", file=sys.stderr)
PY
  [ -x "$DEVBOX_GO" ] || devbox_die "$DEVBOX_GO is missing; rerun host-bootstrap.sh"
}

# Run a fleet deploy script with only the variables it is given, so nothing
# else in the box's settings is read as fleet deploy configuration.
devbox_deploy() {
  (cd "$DEVBOX_REPO" && env -i PATH="$PATH" HOME="${HOME:-/root}" LANG=C.UTF-8 \
    DEPLOY_TRANSPORT=local DEPLOY_INSTANCE="$DEVBOX_INSTANCE" DEPLOY_ZONE="$DEVBOX_ZONE" \
    VMD_SERVICE=superserve-vmd VMD_INSTALL_DIR=/usr/local/bin "$@")
}

# The same flags the fleet deploy workflows build host binaries with.
devbox_build() {
  local out=$1 pkg=$2
  (cd "$DEVBOX_REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-buildvcs=false \
    "$DEVBOX_GO" build -ldflags '-s -w' -trimpath -o "$out" "$pkg") \
    || devbox_die "building $pkg failed"
}
