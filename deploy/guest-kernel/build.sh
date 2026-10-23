#!/usr/bin/env bash
# Build the guest kernel from the committed config, inside an Ubuntu 24.04
# chroot so the build host's packages (glibc included) are never upgraded.
# Needs root on an x86_64 Linux host; works without Docker.
#
#   sudo build.sh <workdir>
#
# Leaves <workdir>/vmlinux-6.1.158 and its .config, and prints the sha256.
# Kernel builds embed a timestamp, so a rebuild's sha256 differs from the
# published artifact's; the config is what must match.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
work=${1:?usage: build.sh <workdir>}
tag=microvm-kernel-6.1.158-15.288.amzn2023
base=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.3-base-amd64.tar.gz

mkdir -p "$work" && cd "$work"
if [ ! -x root/usr/bin/apt-get ]; then
  curl -fsSL -o base.tgz "$base"
  mkdir -p root && tar -xzf base.tgz -C root
  cp /etc/resolv.conf root/etc/resolv.conf
fi
for m in proc sys dev; do mountpoint -q "root/$m" || mount --bind "/$m" "root/$m"; done
trap 'for m in dev sys proc; do umount "root/$m" 2>/dev/null || true; done' EXIT

chroot root bash -c 'command -v gcc >/dev/null || { apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends build-essential flex bison bc libelf-dev libssl-dev git ca-certificates python3 >/dev/null; }'
[ -d root/src/linux ] || chroot root git clone -q --depth 1 --branch "$tag" https://github.com/amazonlinux/linux /src/linux
# A reused workdir must hold exactly the pinned tag: the config check below
# cannot see source edits that leave Kconfig alone.
if ! chroot root git -C /src/linux describe --exact-match --tags HEAD 2>/dev/null | grep -qx "$tag" ||
   [ -n "$(chroot root git -C /src/linux status --porcelain --untracked-files=no)" ]; then
  echo "/src/linux in $work is not a clean checkout of $tag; remove it and rerun" >&2
  exit 1
fi
cp "$here/vmlinux-6.1.158.config" root/src/linux/.config

# olddefconfig must leave the committed config untouched: a difference means
# the source or toolchain moved and the config needs regenerating.
chroot root bash -c 'cd /src/linux && cp .config /src/committed.config && make -s olddefconfig'
if ! diff -u <(grep '^CONFIG_' root/src/committed.config) <(grep '^CONFIG_' root/src/linux/.config); then
  echo "olddefconfig changed the committed config; regenerate it (see README)" >&2
  exit 1
fi

jobs=$(( $(nproc) / 2 )); [ "$jobs" -ge 1 ] || jobs=1
chroot root bash -c "cd /src/linux && nice -n 19 make -s -j$jobs vmlinux"
cp root/src/linux/vmlinux vmlinux-6.1.158
cp root/src/linux/.config vmlinux-6.1.158.config
sha256sum vmlinux-6.1.158
