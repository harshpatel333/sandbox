# Guest kernel

Sandboxes boot a monolithic `vmlinux` (no modules shipped) built from the
Amazon Linux microVM kernel tree. `KERNEL_PATH` in `/etc/sandbox/vmd.env` names
the file vmd boots; only cold boots read it (template builds, revives, backup
restores), while snapshot restores keep the kernel captured in the snapshot.

| | |
|---|---|
| Source | `amazonlinux/linux` tag `microvm-kernel-6.1.158-15.288.amzn2023` |
| Config | `vmlinux-6.1.158.config` (full, resolved) |
| Artifact | `hostprep/vmlinux-6.1.158` in the hostprep buckets |
| sha256 | `9868c342e90b1ad94d12416a52518689ed618082b5db55686119ddf266f78393` |
| Toolchain | Ubuntu 24.04, gcc 13.3 |

## Build

```
sudo deploy/guest-kernel/build.sh /var/tmp/kbuild
```

`build.sh` builds in an Ubuntu 24.04 chroot, so it never upgrades the build
host's packages, and refuses to build if `olddefconfig` would change the
committed config. Builds embed a timestamp, so a rebuild's sha256 will not
match the published one; verify the config instead.

## Changing the config

`vmlinux-6.1.158.config` is Firecracker's 6.1 CI config plus `superserve.config`,
resolved by `make olddefconfig`. To change an option, edit `superserve.config`
(one comment per option saying why it is needed) and regenerate:

1. Take `microvm-kernel-ci-x86_64-6.1.config`, `ci.config`, `pcie.config`,
   `virtio-pmem.config`, `virtio-mem.config` and `vmclock.config` from
   `resources/guest_configs/` in `superserve-ai/firecracker` at `de258de2535f`.
2. In the kernel tree, concatenate them in that order followed by
   `superserve.config` into `.config`, run `make olddefconfig`, and commit the
   result as `vmlinux-6.1.158.config`.
3. Check every option in `superserve.config` survived: `olddefconfig` silently
   drops options whose dependencies are unmet.

Options the platform itself depends on, which any config change must keep:
`IP_PNP` (guest networking from the boot line), `PTP_1588_CLOCK_KVM`
(`/dev/ptp0`, the guest's host-time source), `CGROUP_FREEZER` and the other
cgroup v1 controllers the guest init mounts, `SERIO_I8042` (`reboot=k`),
`DEVTMPFS`, `SWAP` and `FUSE_FS`.
