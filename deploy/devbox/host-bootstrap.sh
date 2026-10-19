#!/bin/bash
# One-time host setup for a devbox: a single sandbox host that is its own
# cell. Runs the fleet's own host bootstrap (deploy/host-bootstrap) with
# devbox parameters, then the steps an operator performs for a fresh cell:
# host identity, the cell's secretsproxy CA, and reflink-backed sandbox
# storage on the data disk. Writes the runtime manifest last, so its presence
# means the host is ready for install-vmd.sh.
#
# Safe to rerun: every step converges, and a rerun verifies what the first
# run installed instead of replacing it.
set -euo pipefail

# shellcheck source=deploy/devbox/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DATA=/mnt/sandbox-data
RESERVE_MIB=${DEVBOX_HOST_RESERVE_MIB:-8192}

check_prerequisites() {
  local cmd
  for cmd in curl python3 ip iptables systemctl systemd-run tar sha256sum sysctl mountpoint; do
    command -v "$cmd" >/dev/null || devbox_die "$cmd is not installed"
  done
  [ "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs ] || devbox_die "VMD needs the unified cgroup v2 hierarchy"
  grep -qwE 'vmx|svm' /proc/cpuinfo \
    || devbox_die "the CPU exposes no virtualization extensions; create the VM with nested virtualization enabled"
  modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
  [ -c /dev/kvm ] || devbox_die "/dev/kvm is missing; the VM needs nested virtualization"
  # KVM_GET_API_VERSION: Firecracker refuses any API version other than 12.
  python3 - <<'PY' || devbox_die "/dev/kvm is not usable"
import fcntl, os, sys
fd = os.open("/dev/kvm", os.O_RDWR | os.O_CLOEXEC)
version = fcntl.ioctl(fd, 0xAE00)
sys.exit(0 if version == 12 else f"unexpected KVM API version {version}")
PY
  [ -e "/dev/disk/by-id/google-$DEVBOX_DATA_DISK_DEVICE" ] \
    || devbox_die "data disk /dev/disk/by-id/google-$DEVBOX_DATA_DISK_DEVICE is not attached"
  [[ "$DEVBOX_FIRECRACKER_SHA256" =~ ^[0-9a-f]{64}$ ]] || devbox_die "DEVBOX_FIRECRACKER_SHA256 must be a SHA-256"
  local digest
  for digest in "$DEVBOX_KERNEL_DIGEST" "$DEVBOX_ROOTFS_DIGEST"; do
    [[ "$digest" =~ ^([0-9a-f]{32}|[0-9a-f]{64})$ ]] || devbox_die "artifact digests must be MD5 or SHA-256: $digest"
  done
  [[ "$DEVBOX_KERNEL_OBJECT" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && "$DEVBOX_ROOTFS_OBJECT" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] \
    || devbox_die "artifact object names must be plain file names"
}

# The fleet bootstrap, rendered from the same template Terraform renders. No
# local SSD here: sandbox storage lives on the data disk (storage below).
run_host_bootstrap() {
  local vars rendered
  BOOTSTRAP_VARS=$(python3 - "$DEVBOX_REGION" "$MEMORY_MIB" "$VCPUS" "$DEVBOX_PROXY_ADDR" <<'PY'
import json, os, sys
region, memory, vcpus, proxy = sys.argv[1:]
print(json.dumps({
    "localssd_script": "",
    "artifact_bucket": os.environ["DEVBOX_ARTIFACT_BUCKET"],
    "kernel_object": os.environ["DEVBOX_KERNEL_OBJECT"],
    "kernel_md5": os.environ["DEVBOX_KERNEL_DIGEST"],
    "rootfs_object": os.environ["DEVBOX_ROOTFS_OBJECT"],
    "rootfs_md5": os.environ["DEVBOX_ROOTFS_DIGEST"],
    "data_disk_device": os.environ["DEVBOX_DATA_DISK_DEVICE"],
    "firecracker_version": os.environ["DEVBOX_FIRECRACKER_VERSION"],
    "firecracker_sha256": os.environ["DEVBOX_FIRECRACKER_SHA256"],
    "vmd_env": {
        "HOST_REGION": region,
        "VMD_SCHEDULABLE_MEMORY_MIB": memory,
        "VMD_SCHEDULABLE_VCPUS": vcpus,
        "SECRETSPROXY_CA_CERT": "/var/lib/secretsproxy/ca.crt",
        "PROXY_HEALTH_URL": f"http://{proxy}/health",
    },
}))
PY
)
  vars=$BOOTSTRAP_VARS
  rendered=$(mktemp)
  printf '%s' "$vars" | python3 "$DEVBOX_REPO/deploy/devbox/render_tftpl.py" \
    "$DEVBOX_REPO/deploy/host-bootstrap/sandbox-host-bootstrap.sh.tftpl" - > "$rendered"
  devbox_log "running the fleet host bootstrap (log: /var/log/sandbox-host-bootstrap.log)"
  if ! bash "$rendered"; then
    rm -f "$rendered"
    tail -n 40 /var/log/sandbox-host-bootstrap.log >&2 || true
    devbox_die "the fleet host bootstrap failed"
  fi
  rm -f "$rendered"
  systemctl start sandbox-data.service || devbox_die "the data disk did not mount; see journalctl -u sandbox-data"
  mountpoint -q "$DATA" || devbox_die "$DATA is not mounted"
}

# The fleet bootstrap installs artifacts and writes vmd.env only when they
# are absent, so a rerun with changed pins would otherwise report a runtime
# the box is not running.
check_rerun_pins() {
  [ -s "$DEVBOX_RUNTIME_JSON" ] || return 0
  python3 "$DEVBOX_REPO/deploy/devbox/devbox_host.py" pin-drift --runtime "$DEVBOX_RUNTIME_JSON" \
    || devbox_die "runtime inputs differ from this box's bootstrap; recreate the box to change them"
}

verify_vmd_env() {
  local key want have expected
  expected=$(printf '%s' "$BOOTSTRAP_VARS" | python3 -c '
import json, sys
for key, value in sorted(json.load(sys.stdin)["vmd_env"].items()):
    print(f"{key}={value}")') || devbox_die "cannot read the bootstrap settings"
  expected+=$(printf '\nKERNEL_PATH=/var/lib/sandbox/kernel/%s\nBASE_ROOTFS_PATH=/var/lib/sandbox/rootfs/base.ext4' "$DEVBOX_KERNEL_OBJECT")
  while IFS='=' read -r key want; do
    have=$(sed -n "s/^$key=//p" /etc/sandbox/vmd.env | tail -n 1)
    [ "$have" = "$want" ] \
      || devbox_die "/etc/sandbox/vmd.env has $key=$have, not $want; settings changed after bootstrap, recreate the box"
  done <<< "$expected"
}

# Run directories and snapshots on the data disk, bind-mounted under the
# canonical paths before VMD on every boot: pause staging and saved snapshots
# clone files, and the boot disk cannot.
install_storage() {
  cat > /usr/local/bin/sandbox-data-dirs <<'SH'
#!/bin/bash
set -euo pipefail
mountpoint -q /mnt/sandbox-data
xfs_info /mnt/sandbox-data | grep -q 'reflink=1'
for d in rundir snapshots; do
  mkdir -p "/mnt/sandbox-data/sandbox/$d" "/var/lib/sandbox/$d"
  mountpoint -q "/var/lib/sandbox/$d" || mount --bind "/mnt/sandbox-data/sandbox/$d" "/var/lib/sandbox/$d"
done
SH
  chmod 0755 /usr/local/bin/sandbox-data-dirs
  cat > /etc/systemd/system/sandbox-data-dirs.service <<'UNIT'
[Unit]
Description=Bind the sandbox run and snapshot directories onto the data disk
Requires=sandbox-data.service
After=sandbox-data.service
Before=superserve-vmd.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/sandbox-data-dirs

[Install]
WantedBy=multi-user.target
UNIT
  printf '[Unit]\nRequires=sandbox-data-dirs.service\nAfter=sandbox-data-dirs.service\n' \
    > /etc/systemd/system/superserve-vmd.service.d/sandbox-data-dirs.conf
  systemctl daemon-reload
  systemctl enable --quiet sandbox-data-dirs.service
  systemctl start sandbox-data-dirs.service || devbox_die "sandbox storage did not mount; see journalctl -u sandbox-data-dirs"
}

verify_reflink() {
  local dir=/var/lib/sandbox/snapshots probe
  probe=$(mktemp "$dir/.reflink-probe.XXXXXX")
  head -c 1048576 /dev/urandom > "$probe"
  if ! cp --reflink=always "$probe" "$probe.clone" 2>/dev/null; then
    rm -f "$probe" "$probe.clone"
    devbox_die "$dir cannot reflink; saved snapshots and forks need XFS with reflink"
  fi
  rm -f "$probe" "$probe.clone"
}

verify_artifacts() {
  local fc=/usr/local/bin/firecracker kernel=/var/lib/sandbox/kernel/$DEVBOX_KERNEL_OBJECT sum
  [ "$(sha256sum < "$fc" | cut -d' ' -f1)" = "$DEVBOX_FIRECRACKER_SHA256" ] \
    || devbox_die "$fc is not Firecracker $DEVBOX_FIRECRACKER_VERSION; pins changed after bootstrap, recreate the box"
  grep -qa serial-console-cap "$fc" || devbox_die "$fc lacks the serial-console capability VMD requires"
  "$fc" --version >/dev/null || devbox_die "$fc does not run"
  case ${#DEVBOX_KERNEL_DIGEST} in 64) sum=sha256sum ;; *) sum=md5sum ;; esac
  [ "$("$sum" < "$kernel" | cut -d' ' -f1)" = "$DEVBOX_KERNEL_DIGEST" ] \
    || devbox_die "$kernel does not match DEVBOX_KERNEL_DIGEST; pins changed after bootstrap, recreate the box"
  [ -s /var/lib/sandbox/rootfs/base.ext4 ] || devbox_die "the base rootfs is missing"
  # Until the first VMD install injects boxd, the rootfs is still the pinned
  # image; a leftover from an interrupted run with other pins is not.
  if [ ! -s "$DEVBOX_RUNTIME_JSON" ]; then
    case ${#DEVBOX_ROOTFS_DIGEST} in 64) sum=sha256sum ;; *) sum=md5sum ;; esac
    [ "$("$sum" < /var/lib/sandbox/rootfs/base.ext4 | cut -d' ' -f1)" = "$DEVBOX_ROOTFS_DIGEST" ] \
      || devbox_die "the base rootfs does not match DEVBOX_ROOTFS_DIGEST; recreate the box"
  fi
}

install_go() {
  if [ -x "$DEVBOX_GO" ] && [ "$("$DEVBOX_GO" env GOVERSION 2>/dev/null)" = "$DEVBOX_GO_VERSION" ]; then
    return
  fi
  local tmp
  tmp=$(mktemp -d)
  curl -sfL --retry 5 "https://go.dev/dl/$DEVBOX_GO_VERSION.linux-amd64.tar.gz" -o "$tmp/go.tgz" \
    || devbox_die "downloading $DEVBOX_GO_VERSION failed"
  echo "$DEVBOX_GO_SHA256  $tmp/go.tgz" | sha256sum -c - >/dev/null \
    || devbox_die "$DEVBOX_GO_VERSION does not match DEVBOX_GO_SHA256"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/go.tgz"
  rm -rf "$tmp"
}

install_identity() {
  local project instance_id
  project=$(devbox_metadata project/project-id)
  instance_id=$(devbox_metadata instance/id)
  HOST_ID=$(python3 "$DEVBOX_REPO/deploy/devbox/devbox_host.py" identity \
    --project "$project" --instance-id "$instance_id" --slot "$DEVBOX_NAME") \
    || devbox_die "host identity installation failed"
  systemctl daemon-reload
}

# A devbox is a brand-new cell, so it mints its own secretsproxy CA, written
# as the daemon's dynamic user exactly as an operator restores a cell's CA.
# Only a box that never bootstrapped is a fresh cell: a missing or partial
# pair on a box that ran is lost trust, which templates and guests still hold
# the old CA for.
install_secretsproxy_ca() {
  local dir=/var/lib/secretsproxy crt=0 key=0
  [ -e "$dir/ca.crt" ] && crt=1
  [ -e "$dir/ca.key" ] && key=1
  if [ "$crt$key" = 11 ]; then
    return
  fi
  [ "$crt$key" = 00 ] || devbox_die "only one of $dir/ca.crt and ca.key exists; refusing to replace the cell CA (recreate the box)"
  if [ -e "$DEVBOX_RUNTIME_JSON" ] || [ -e /var/lib/sandbox/vmd.db ]; then
    devbox_die "the secretsproxy CA is missing on a box that already bootstrapped; recreate the box"
  fi
  install -d -m 0755 "$DEVBOX_STATE/bin"
  devbox_build "$DEVBOX_STATE/bin/gen-secretsproxy-ca" ./scripts/gen-secretsproxy-ca
  systemd-run --quiet --wait --collect --unit=secretsproxy-ca-init \
    --property=User=superserve-secretsproxy --property=DynamicUser=yes \
    --property=StateDirectory=secretsproxy --property=StateDirectoryMode=0700 \
    "$DEVBOX_STATE/bin/gen-secretsproxy-ca" -cert "$dir/ca.crt" -key "$dir/ca.key" \
    || devbox_die "generating the secretsproxy CA failed"
  if [ ! -s "$dir/ca.crt" ] || [ ! -s "$dir/ca.key" ]; then
    devbox_die "the secretsproxy CA was not written"
  fi
}

write_runtime() {
  local revision
  revision=$(devbox_revision)
  install -d -m 0755 "$DEVBOX_STATE"
  python3 "$DEVBOX_REPO/deploy/devbox/devbox_host.py" runtime --out "$DEVBOX_RUNTIME_JSON" \
    --revision "$revision" --instance "$DEVBOX_INSTANCE" --zone "$DEVBOX_ZONE" \
    --region "$DEVBOX_REGION" --bucket "$DEVBOX_ARTIFACT_BUCKET" \
    --firecracker-version "$DEVBOX_FIRECRACKER_VERSION" --firecracker-sha256 "$DEVBOX_FIRECRACKER_SHA256" \
    --kernel-object "$DEVBOX_KERNEL_OBJECT" --kernel-digest "$DEVBOX_KERNEL_DIGEST" \
    --rootfs-object "$DEVBOX_ROOTFS_OBJECT" --rootfs-digest "$DEVBOX_ROOTFS_DIGEST" \
    --data-disk-device "$DEVBOX_DATA_DISK_DEVICE" --go-version "$DEVBOX_GO_VERSION" \
    --memory-mib "$MEMORY_MIB" --vcpus "$VCPUS"
}

main() {
  devbox_require_root
  devbox_load_env
  devbox_instance
  check_prerequisites
  # A rerun keeps the capacity sized at create (memory reported by the kernel
  # drifts across updates); explicit overrides still apply and are verified.
  local memory=${DEVBOX_VMD_SCHEDULABLE_MEMORY_MIB:-} vcpus=${DEVBOX_VMD_SCHEDULABLE_VCPUS:-}
  if [ -f /etc/sandbox/vmd.env ]; then
    memory=${memory:-$(sed -n 's/^VMD_SCHEDULABLE_MEMORY_MIB=//p' /etc/sandbox/vmd.env | tail -n 1)}
    vcpus=${vcpus:-$(sed -n 's/^VMD_SCHEDULABLE_VCPUS=//p' /etc/sandbox/vmd.env | tail -n 1)}
  fi
  read -r MEMORY_MIB VCPUS < <(python3 "$DEVBOX_REPO/deploy/devbox/devbox_host.py" capacity \
    --nproc "$(nproc)" --reserve-mib "$RESERVE_MIB" --memory-mib "$memory" --vcpus "$vcpus") \
    || devbox_die "cannot size schedulable capacity"
  devbox_log "host $DEVBOX_INSTANCE ($DEVBOX_ZONE): $MEMORY_MIB MiB, $VCPUS vCPUs schedulable"
  check_rerun_pins
  run_host_bootstrap
  verify_vmd_env
  install_storage
  verify_reflink
  verify_artifacts
  install_go
  install_identity
  install_secretsproxy_ca
  write_runtime
  devbox_log "host bootstrapped as $HOST_ID; runtime manifest at $DEVBOX_RUNTIME_JSON"
}

main "$@"
