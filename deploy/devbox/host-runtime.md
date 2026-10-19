# Devbox host runtime

A devbox is one GCE VM that is its own sandbox cell. Three entry points
prepare and update its host runtime. Each runs as root on the box from the
checked-out sandbox revision (`/opt/superserve-devbox/src/sandbox`), is safe
to rerun, and exits non-zero with a message naming what is missing.

| Script | Runs | Does |
| --- | --- | --- |
| `host-bootstrap.sh` | once, at create | Checks KVM and the data disk, runs the fleet host bootstrap, binds sandbox storage onto the data disk, verifies reflink and the pinned artifacts, installs Go, the host identity and the cell's secretsproxy CA, then writes `/var/lib/superserve-devbox/runtime.json` |
| `install-vmd.sh` | create and upgrade | Builds `vmd`, `boxd`, `template-builder` and `secretsproxy` and installs them with `deploy-vmd.py` |
| `install-proxy.sh` | create and upgrade | Builds `proxy` and installs it with `deploy-proxy-legacy.py` |

Both install scripts run the fleet deploy scripts with `DEPLOY_TRANSPORT=local`,
so the box goes through the same on-host steps as a fleet host:
compatibility guards, the socket-held restart, boxd injection into the base
rootfs, and readiness gates. An upgrade swaps binaries without recreating
sandboxes, exactly as a fleet deploy does.

## Order

The install scripts depend on the local services:

* VMD exits at startup if it cannot reach the `DATABASE_URL` it is given.
* On a fresh host, the VMD deploy starts secretsproxy and waits for it to be
  healthy before it activates VMD. Secretsproxy fetches its JWKS and
  revocations from the API before it reports healthy.

So the order is:

* **Create:** `host-bootstrap` → `services-up` → `install-api` → `install-vmd`
  → `install-proxy` → `seed-base` → `install-ui`. `seed-base` activates the
  host, which needs VMD's first heartbeat, and builds the base template.
* **Upgrade:** `install-vmd` → `install-proxy` → local services and API →
  console, which is the order production deploys use. Secretsproxy restarts
  against the API that is already running.

## Inputs

`/etc/superserve-devbox/devbox.env` (non-secret, written by CI):

| Variable | Default | Meaning |
| --- | --- | --- |
| `DEVBOX_NAME`, `DEVBOX_ID` | required | Box name and id |
| `DEVBOX_SANDBOX_SHA` | unset | Selected revision. When it is set, the scripts refuse a checkout at any other revision |
| `DEVBOX_BUCKET` | `superserve-devbox-$DEVBOX_ID` | The box's bucket. VMD uploads template builds and pause backups to it |
| `DEVBOX_ARTIFACT_BUCKET` | `rayai-dev-devbox-hostprep` | Shared read-only bucket whose `hostprep/` prefix holds the kernel and rootfs objects. The box reads it with its own identity |
| `DEVBOX_DATA_DISK_DEVICE` | `devbox-data` | GCE device name of the data disk, as in `/dev/disk/by-id/google-<name>` |
| `DEVBOX_FIRECRACKER_VERSION`, `DEVBOX_FIRECRACKER_SHA256` | `runtime-pins.env` | Firecracker release and binary digest |
| `DEVBOX_KERNEL_OBJECT`, `DEVBOX_KERNEL_DIGEST` | `runtime-pins.env` | Guest kernel object under `hostprep/` and its MD5 or SHA-256 |
| `DEVBOX_ROOTFS_OBJECT`, `DEVBOX_ROOTFS_DIGEST` | `runtime-pins.env` | Base rootfs object under `hostprep/` and its MD5 or SHA-256 |
| `DEVBOX_VMD_SCHEDULABLE_MEMORY_MIB`, `DEVBOX_VMD_SCHEDULABLE_VCPUS` | machine memory minus `DEVBOX_HOST_RESERVE_MIB` (8192), and all vCPUs | Capacity VMD advertises |
| `DEVBOX_PROXY_ADDR` | `127.0.0.1:8090` | Proxy listener. Fixed when the box is created, because VMD probes its health there |
| `DEVBOX_API_URL` | `http://127.0.0.1:8080` | Control plane VMD and secretsproxy call |
| `DEVBOX_CONSOLE_ORIGIN` | `http://localhost:3000` | Browser origin allowed to use the proxy data plane |

`runtime-pins.env` holds the versions staging runs. Explicit inputs win over
it, and `runtime.json` records what was installed.

The kernel and rootfs are read from `gs://$DEVBOX_ARTIFACT_BUCKET/hostprep/`
with the box's own identity. Boxes can read that bucket but not write to it,
and it is seeded once from the staging host's verified files. Firecracker comes from
the public `superserve-ai/firecracker` release and Go from `go.dev`. Each is
verified against its pin.

Shared local secrets are read from `/etc/superserve-devbox/local-secrets.env`.
`services-up.sh` generates them once on the data disk, and that path is a
symlink to the data-disk copy:

* `INTERNAL_API_TOKEN`: VMD, and secretsproxy as `DAEMON_AUTH_TOKEN`. It must
  equal the API's token.
* `DATABASE_URL`: VMD and the secretsproxy audit sink.
* `SANDBOX_ACCESS_TOKEN_SEED`: the proxy data plane, shared with the API.

## What it configures

* **Fleet bootstrap:** `deploy/host-bootstrap/sandbox-host-bootstrap.sh.tftpl`,
  rendered with `render_tftpl.py` instead of Terraform and run without local
  SSD. It covers:
  * forwarding, the neighbour-table and conntrack sizing, and their
    persistent sysctl files;
  * the XFS (reflink) data disk mounted at `/mnt/sandbox-data`;
  * the kernel, rootfs and Firecracker;
  * `/etc/sandbox/vmd.env`, with region, capacity, the secretsproxy CA path
    and `PROXY_HEALTH_URL`.
* **Sandbox storage:** `/var/lib/sandbox/{rundir,snapshots}` are bind-mounted
  from the data disk before VMD starts on every boot
  (`sandbox-data-dirs.service`). Saved snapshots and forks clone files, and
  the boot disk cannot.
* **Identity:** `HOST_ID` is `<name>-<32 hex>`, with a fresh incarnation,
  bound to this VM's project and instance id. It uses the same files and
  rules as `deploy/install-host-identity.py`. A rerun reuses it, and a record
  from another VM is refused.
* **Secretsproxy CA:** minted once, as for a new cell. A half-present pair is
  never repaired.
* **DNS and egress:** as on staging. Guests resolve through their own
  nameservers, and VMD masquerades guest traffic out of the host interface.
  VMD owns the `filter FORWARD`, `nat PREROUTING` and `nat POSTROUTING` rules
  for `veth+` and `10.11.0.0/16`.
* **Proxy:** plain HTTP on `127.0.0.1:8090`. Sandbox hosts are
  `http://<port>-<sandbox-id>.sandbox.localhost:8090`, and the data plane is
  `boxd-<sandbox-id>`. `PROXY_PUBLIC_SCHEME=http` keeps preview-link
  redirects on http. Private preview links rely on browsers keeping the
  Secure preview cookie on `*.localhost`. Chromium-based browsers and recent
  Firefox do; Safari may not. The fleet units are unchanged; drop-ins rebind them to
  loopback.

## Docker on the same host

The local services run in Docker. Docker stays out of the kernel's netfilter
tables entirely, so the only `FORWARD` and NAT rules on the box are VMD's:

* `/etc/docker/daemon.json` sets `"iptables": false`, `"ip6tables": false`
  and `"bridge": "none"` before Docker is installed. There is no `docker0`
  and no Docker chains.
* Containers use host networking and bind `127.0.0.1` only.
* Nothing else binds the ports VMD, secretsproxy and the proxy use: `50051`,
  `9090`, `19080`, `19443`, `19090`, `9443`, `8090` and `5008`.
* No `INPUT` drop policy is configured, so loopback stays open.

## On-box checks

These need a real box (nested KVM). The single end-to-end smoke covers them:

* `host-bootstrap.sh` exits 0, and rerunning it is a no-op that also exits 0.
* `test -c /dev/kvm`, and `/usr/local/bin/firecracker --version` reports the
  pinned release.
* `findmnt /mnt/sandbox-data` shows XFS. `findmnt /var/lib/sandbox/snapshots`
  is a bind of `/mnt/sandbox-data/sandbox/snapshots`.
  `xfs_info /mnt/sandbox-data | grep reflink=1` matches.
* `sysctl net.ipv4.ip_forward net.ipv4.neigh.default.gc_thresh3 net.netfilter.nf_conntrack_max`
  prints `1`, `32768` and `4194304`.
* `jq . /var/lib/superserve-devbox/runtime.json` shows `host_id`,
  `host_region`, `vmd_grpc_address`, the pins and the capacity. `HOST_ID` in `/etc/sandbox/host-identity.env` matches.
* After `install-vmd.sh`, the following are active:
  `systemctl is-active superserve-vmd superserve-vmd.socket superserve-secretsproxy`.
  `journalctl -u superserve-vmd` shows `gRPC serving requests`, and the host
  row appears in `hosts` as `provisioning`.
* After `install-proxy.sh`, `curl -sf http://127.0.0.1:8090/health` succeeds.
  `ss -ltn` shows the proxy only on `127.0.0.1:8090` and `127.0.0.1:5008`.
* A sandbox created through the console runs. Its preview opens at
  `http://<port>-<id>.sandbox.localhost:8090`, and pause, resume and fork
  work.
* Once the host is active, a template build completes on the box.
* Rerunning `install-vmd.sh` and `install-proxy.sh` at a different revision
  keeps running and paused sandboxes.
* `iptables -S FORWARD` shows only VMD's `veth+` rules, and no Docker chains.
  Guest egress works with the services running.
