"""Devbox host runtime helpers: template rendering, identity, capacity, manifest."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
DEPLOY = HERE.parent


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


render_tftpl = load("render_tftpl", HERE / "render_tftpl.py")
devbox_host = load("devbox_host", HERE / "devbox_host.py")

BOOTSTRAP = (DEPLOY / "host-bootstrap/sandbox-host-bootstrap.sh.tftpl").read_text()
FLEET_VARS = {
    "localssd_script": (DEPLOY / "host-bootstrap/sandbox-localssd.sh").read_text(),
    "artifact_bucket": "example-bucket",
    "kernel_object": "vmlinux-example",
    "kernel_md5": "0" * 32,
    "rootfs_object": "base.ext4",
    "rootfs_md5": "1" * 32,
    "data_disk_device": "example-data",
    "vmd_env": {"VMD_SCHEDULABLE_VCPUS": "8", "HOST_REGION": "us-central1"},
    "firecracker_version": "v0.0.0",
    "firecracker_sha256": "2" * 64,
}


class RenderTests(unittest.TestCase):
    # Expected outputs are what Terraform's templatefile() produces for the
    # same input, so the box renders the fleet bootstrap exactly as fleet
    # hosts receive it.
    def test_matches_terraform_semantics(self):
        cases = [
            ('a\n%{ if x != "" ~}\nX=${x}\n%{ endif ~}\n\nb\n', {"x": "1"}, "a\nX=1\n\nb\n"),
            ('a\n%{ if x != "" ~}\nX=${x}\n%{ endif ~}\n\nb\n', {"x": ""}, "a\n\nb\n"),
            ("%{ for k, v in m ~}\n${k}=${v}\n%{ endfor ~}\nend\n", {"m": {"b": "2", "a": "1"}}, "a=1\nb=2\nend\n"),
            ("s:\n  %{ for a in l ~}\n    x ${a}\n  %{ endfor ~}\ndone\n", {"l": ["1", "2"]},
             "s:\n      x 1\n      x 2\n  done\n"),
            ("echo $${HOME} %%{ x } $1 ${y}\n", {"y": "Y"}, "echo ${HOME} %{ x } $1 Y\n"),
            ('%{ if x == "" ~}\nempty\n%{ else ~}\nset ${x}\n%{ endif ~}\n', {"x": ""}, "empty\n"),
            ('%{ if x == "" ~}\nempty\n%{ else ~}\nset ${x}\n%{ endif ~}\n', {"x": "v"}, "set v\n"),
        ]
        for template, variables, expected in cases:
            with self.subTest(template=template, variables=variables):
                self.assertEqual(render_tftpl.render(template, variables), expected)

    def test_unsupported_or_unbound_template_fails(self):
        for template, variables in [
            ("${upper(x)}", {"x": "a"}),
            ("${missing}", {}),
            ("%{ if x }y%{ endif }", {"x": "a"}),
            ('%{ if x != "" }y', {"x": "a"}),
            ("%{~ if x }", {"x": "a"}),
            ("${m}", {"m": {"a": "b"}}),
        ]:
            with self.subTest(template=template), self.assertRaises(render_tftpl.TemplateError):
                render_tftpl.render(template, variables)

    def test_fleet_bootstrap_keeps_local_ssd_and_devbox_omits_it(self):
        fleet = render_tftpl.render(BOOTSTRAP, FLEET_VARS)
        self.assertIn("sandbox-localssd.service", fleet)
        self.assertIn("mdadm --create", fleet)
        devbox = render_tftpl.render(BOOTSTRAP, dict(FLEET_VARS, localssd_script=""))
        self.assertNotIn("localssd", devbox)
        self.assertIn("/dev/disk/by-id/google-example-data", devbox)
        self.assertIn("HOST_REGION=us-central1\nVMD_SCHEDULABLE_VCPUS=8\n", devbox)
        for script in (fleet, devbox):
            syntax = subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True)
            self.assertEqual(syntax.returncode, 0, syntax.stderr)

    def test_artifact_fetch_verifies_md5_and_sha256_pins(self):
        rendered = render_tftpl.render(BOOTSTRAP, FLEET_VARS)
        fetch = rendered[rendered.index("fetch() {"):rendered.index("\n}\n", rendered.index("fetch() {")) + 3]
        payload = b"example artifact\n"
        md5 = subprocess.run(["md5sum"], input=payload, capture_output=True).stdout.split()[0].decode()
        sha256 = subprocess.run(["sha256sum"], input=payload, capture_output=True).stdout.split()[0].decode()
        with tempfile.TemporaryDirectory() as tmp:
            Path(tmp, "payload").write_bytes(payload)
            stubs = f"""
curl() {{
    case "$*" in *metadata.google.internal*) echo '{{"access_token": "t"}}'; return ;; esac
    while [ $# -gt 0 ]; do [ "$1" = -o ] && cp {tmp}/payload "$2"; shift; done
}}
sleep() {{ :; }}
"""
            for digest, ok in ((md5, True), (sha256, True), ("0" * 32, False), ("0" * 64, False)):
                with self.subTest(digest=digest):
                    target = Path(tmp, "artifact")
                    target.unlink(missing_ok=True)
                    script = stubs + fetch + f"fetch object {target} {digest}\n"
                    result = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
                    self.assertEqual(result.returncode == 0, ok, result.stderr)
                    self.assertEqual(target.exists(), ok)


class HostTests(unittest.TestCase):
    def test_identity_is_minted_once_and_reused(self):
        with tempfile.TemporaryDirectory() as root:
            first = devbox_host.install_identity(root, "example-project", "100", "example-box")
            self.assertRegex(first["host_id"], r"^example-box-[0-9a-f]{32}$")
            self.assertEqual(devbox_host.install_identity(root, "example-project", "100", "example-box"), first)
            state = Path(root, "etc/sandbox/host-identity.json")
            self.assertEqual(json.loads(state.read_text()), first)
            self.assertEqual(state.stat().st_mode & 0o777, 0o600)
            self.assertEqual(Path(root, "etc/sandbox/host-identity.env").read_text(),
                             f"HOST_ID={first['host_id']}\nHOST_IDENTITY_FILE=/etc/sandbox/host-identity.json\n")
            self.assertTrue(Path(root, "etc/systemd/system/superserve-vmd.service.d/identity.conf").exists())

    def test_identity_from_another_machine_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            devbox_host.install_identity(root, "example-project", "100", "example-box")
            with self.assertRaises(ValueError):
                devbox_host.install_identity(root, "example-project", "101", "example-box")

    def test_lost_identity_record_on_a_used_box_is_refused(self):
        for trace in ("etc/sandbox/host-identity.env", "var/lib/superserve-devbox/runtime.json",
                      "var/lib/sandbox/vmd.db"):
            with self.subTest(trace=trace), tempfile.TemporaryDirectory() as root:
                path = Path(root, trace)
                path.parent.mkdir(parents=True)
                path.write_text("")
                with self.assertRaises(ValueError):
                    devbox_host.install_identity(root, "example-project", "100", "example-box")
                self.assertFalse(Path(root, "etc/sandbox/host-identity.json").exists())

    def test_pin_drift_names_every_changed_runtime_input(self):
        manifest = {"artifacts": {
            "source": "gs://example-artifacts/hostprep/",
            "firecracker": {"version": "v0.0.0", "sha256": "2" * 64},
            "kernel": {"object": "vmlinux-example", "digest": "3" * 64},
            "rootfs": {"object": "base.ext4", "digest": "4" * 64},
        }}
        env = {"DEVBOX_ARTIFACT_BUCKET": "example-artifacts", "DEVBOX_FIRECRACKER_VERSION": "v0.0.0",
               "DEVBOX_FIRECRACKER_SHA256": "2" * 64, "DEVBOX_KERNEL_OBJECT": "vmlinux-example",
               "DEVBOX_KERNEL_DIGEST": "3" * 64, "DEVBOX_ROOTFS_OBJECT": "base.ext4",
               "DEVBOX_ROOTFS_DIGEST": "4" * 64}
        self.assertEqual(devbox_host.pin_drift(manifest, env), [])
        changed = dict(env, DEVBOX_KERNEL_OBJECT="vmlinux-other", DEVBOX_ROOTFS_DIGEST="5" * 64)
        self.assertEqual(devbox_host.pin_drift(manifest, changed), ["DEVBOX_KERNEL_OBJECT", "DEVBOX_ROOTFS_DIGEST"])

    def test_capacity_reserves_host_memory_and_honours_overrides(self):
        meminfo = "MemTotal:       32873024 kB\nMemFree:  1 kB\n"
        self.assertEqual(devbox_host.capacity(meminfo, "8", "8192"), (32102 - 8192, 8))
        self.assertEqual(devbox_host.capacity(meminfo, "8", "8192", "4096", "16"), (4096, 16))
        with self.assertRaises(ValueError):
            devbox_host.capacity(meminfo, "8", "40000")

    def test_runtime_manifest_records_installed_versions_and_identity(self):
        with tempfile.TemporaryDirectory() as root:
            identity = devbox_host.install_identity(root, "example-project", "100", "example-box")
            out = Path(root, "runtime.json")
            devbox_host.main([
                "runtime", "--root", root, "--out", str(out), "--revision", "a" * 40,
                "--instance", "example-box", "--zone", "us-central1-a", "--region", "us-central1",
                "--bucket", "example-bucket", "--firecracker-version", "v0.0.0",
                "--firecracker-sha256", "2" * 64, "--kernel-object", "vmlinux-example",
                "--kernel-digest", "3" * 64, "--rootfs-object", "base.ext4", "--rootfs-digest", "4" * 64,
                "--data-disk-device", "example-box-data", "--go-version", "go0.0.0",
                "--memory-mib", "4096", "--vcpus", "8",
            ])
            manifest = json.loads(out.read_text())
        self.assertEqual(manifest["schema_version"], 1)
        self.assertEqual(manifest["host_id"], identity["host_id"])
        self.assertEqual(manifest["host_region"], "us-central1")
        self.assertNotIn("incarnation_id", json.dumps(manifest))
        self.assertEqual(manifest["artifacts"]["firecracker"]["version"], "v0.0.0")
        self.assertEqual(manifest["artifacts"]["kernel"]["path"], "/var/lib/sandbox/kernel/vmlinux-example")
        self.assertEqual(manifest["artifacts"]["source"], "gs://example-bucket/hostprep/")
        self.assertEqual(manifest["capacity"], {"memory_mib": 4096, "vcpus": 8})


if __name__ == "__main__":
    unittest.main()
