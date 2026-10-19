#!/usr/bin/env python3
"""Host-side helpers for deploy/devbox/host-bootstrap.sh.

identity  install this host's identity, or verify the one already installed
capacity  schedulable memory and vCPUs to advertise for this machine
runtime   write the runtime manifest the devbox workflows merge into the box
          manifest
"""
import argparse
import datetime
import importlib.util
import json
import os
from pathlib import Path
import re
import tempfile

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("install_host_identity", HERE.parent / "install-host-identity.py")
install_host_identity = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(install_host_identity)

SCHEMA_VERSION = 1


def write_atomic(path, text, mode):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


def install_identity(root, project, instance_id, slot):
    """A single-host cell has no operator rebind flow: the identity is minted
    once for this VM and reused on every rerun. A record naming another VM
    means the disk was cloned, which must never be adopted."""
    root = Path(root)
    state = root / install_host_identity.STATE.lstrip("/")
    stored = json.loads(state.read_text()) if state.exists() else None
    identity = install_host_identity.choose_identity(project, instance_id, slot, stored)
    for path, text in install_host_identity.identity_files(identity).items():
        target = root / path.lstrip("/")
        if not target.exists() or target.read_text() != text:
            write_atomic(target, text, 0o644 if path == install_host_identity.DROPIN else 0o600)
    return identity


def capacity(meminfo, nproc, reserve_mib, memory_mib=None, vcpus=None):
    """Advertise what is left after the box's own services. Overrides win so
    a box can be sized deliberately; zero or negative capacity is an error."""
    total_kib = int(re.search(r"^MemTotal:\s+(\d+) kB$", meminfo, re.M).group(1))
    memory = int(memory_mib) if memory_mib else total_kib // 1024 - int(reserve_mib)
    cpus = int(vcpus) if vcpus else int(nproc)
    if memory <= 0 or cpus <= 0 or memory > 2**31 - 1 or cpus > 2**31 - 1:
        raise ValueError(f"schedulable capacity must be positive: memory {memory} MiB, {cpus} vCPUs")
    return memory, cpus


def runtime_manifest(args, identity):
    return {
        "schema_version": SCHEMA_VERSION,
        "bootstrapped_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "bootstrap_revision": args.revision,
        "host": {
            "host_id": identity["host_id"],
            "incarnation_id": identity["incarnation_id"],
            "instance": args.instance,
            "instance_id": identity["instance_id"],
            "project": identity["project_id"],
            "zone": args.zone,
            "region": args.region,
        },
        "artifacts": {
            "source": f"gs://{args.bucket}/hostprep/",
            "firecracker": {"version": args.firecracker_version, "sha256": args.firecracker_sha256,
                            "path": "/usr/local/bin/firecracker"},
            "kernel": {"object": args.kernel_object, "digest": args.kernel_digest,
                       "path": f"/var/lib/sandbox/kernel/{args.kernel_object}"},
            "rootfs": {"object": args.rootfs_object, "digest": args.rootfs_digest,
                       "path": "/var/lib/sandbox/rootfs/base.ext4"},
        },
        "capacity": {"memory_mib": args.memory_mib, "vcpus": args.vcpus},
        "storage": {"data_disk_device": args.data_disk_device, "mount": "/mnt/sandbox-data", "reflink": True},
        "go": args.go_version,
    }


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="command", required=True)

    ident = sub.add_parser("identity")
    ident.add_argument("--root", default="/")
    for name in ("project", "instance-id", "slot"):
        ident.add_argument("--" + name, required=True)

    cap = sub.add_parser("capacity")
    cap.add_argument("--meminfo", default="/proc/meminfo")
    cap.add_argument("--nproc", required=True)
    cap.add_argument("--reserve-mib", required=True)
    cap.add_argument("--memory-mib", default="")
    cap.add_argument("--vcpus", default="")

    run = sub.add_parser("runtime")
    run.add_argument("--root", default="/")
    run.add_argument("--out", required=True)
    for name in ("revision", "instance", "zone", "region", "bucket", "firecracker-version",
                 "firecracker-sha256", "kernel-object", "kernel-digest", "rootfs-object",
                 "rootfs-digest", "data-disk-device", "go-version"):
        run.add_argument("--" + name, required=True)
    run.add_argument("--memory-mib", type=int, required=True)
    run.add_argument("--vcpus", type=int, required=True)

    args = p.parse_args(argv)
    if args.command == "identity":
        print(install_identity(args.root, args.project, args.instance_id, args.slot)["host_id"])
    elif args.command == "capacity":
        memory, cpus = capacity(Path(args.meminfo).read_text(), args.nproc, args.reserve_mib,
                                args.memory_mib, args.vcpus)
        print(memory, cpus)
    else:
        state = Path(args.root) / install_host_identity.STATE.lstrip("/")
        identity = json.loads(state.read_text())
        write_atomic(Path(args.out), json.dumps(runtime_manifest(args, identity), indent=2) + "\n", 0o644)


if __name__ == "__main__":
    main()
