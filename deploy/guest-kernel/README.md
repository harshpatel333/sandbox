# Guest kernel

Sandboxes boot a monolithic `vmlinux` (no modules shipped) built from the
Amazon Linux microVM kernel tree. `KERNEL_PATH` in `/etc/sandbox/vmd.env` names
the file vmd boots; only cold boots read it (template builds, revives, backup
restores), while snapshot restores keep the kernel captured in the snapshot.

| | |
|---|---|
| Source | `amazonlinux/linux` tag `microvm-kernel-6.1.158-15.288.amzn2023`, commit `c2b7f640ec01cb093c6ba9e08b7ebcb400741bb4` |
| Config | Firecracker's 6.1 CI config plus `superserve.config` |
| Artifact | `hostprep/vmlinux-6.1.158`, with its resolved config beside it as `vmlinux-6.1.158.config` |
| sha256 | `9868c342e90b1ad94d12416a52518689ed618082b5db55686119ddf266f78393` |
| Toolchain | Ubuntu 24.04, gcc 13.3 |

## Rebuilding

From a fresh clone of the source at that commit, on Ubuntu 24.04:

1. Concatenate, in this order, `microvm-kernel-ci-x86_64-6.1.config`,
   `ci.config`, `pcie.config`, `virtio-pmem.config`, `virtio-mem.config` and
   `vmclock.config` from `resources/guest_configs/` in `superserve-ai/firecracker`
   at `de258de2535f`, then `superserve.config`, into `.config`.
2. Run `make olddefconfig` and check every option in `superserve.config`
   survived: `olddefconfig` silently drops options whose dependencies are unmet.
   The result should match the published `vmlinux-6.1.158.config`.
3. Run `make vmlinux`. Builds embed a timestamp, so the sha256 will differ from
   the published artifact's; the config is what must match.

To change an option, edit `superserve.config` with a comment saying why it is
needed, rebuild, and publish the kernel and its resolved config together.

Options the platform itself depends on, which any config change must keep:
`IP_PNP` (guest networking from the boot line), `PTP_1588_CLOCK_KVM`
(`/dev/ptp0`, the guest's host-time source), `CGROUP_FREEZER` and the other
cgroup v1 controllers the guest init mounts, `SERIO_I8042` (`reboot=k`),
`DEVTMPFS`, `SWAP` and `FUSE_FS`.
