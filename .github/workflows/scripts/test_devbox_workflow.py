"""Exercise the devbox create workflow against a mocked gcloud."""

import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import devbox_contract as contract
import devbox_workflow as workflow


SANDBOX_SHA = "1" * 40
UI_SHA = "2" * 40
NEW_SHA = "3" * 40
PROFILE = {
    "schema_version": 1, "project": "rayai-dev", "zone": "us-central1-a", "region": "us-central1",
    "resource_name_prefix": "devbox-", "labels": {"managed-by": "superserve-devbox"},
    "instance": {"machine_type": "n2-standard-8", "enable_nested_virtualization": True,
                 "image_project": "ubuntu-os-cloud", "image_family": "ubuntu-2404-lts-amd64",
                 "network": "superserve-devbox", "subnetwork": "superserve-devbox-usc1", "external_ip": False,
                 "network_tags": ["superserve-devbox"],
                 "service_account": "devbox-vm@example-project.iam.gserviceaccount.com",
                 "scopes": ["https://www.googleapis.com/auth/cloud-platform"],
                 "metadata": {"enable-oslogin": "TRUE", "block-project-ssh-keys": "TRUE"}},
    "boot_disk": {"type": "pd-balanced", "size_gb": 50, "auto_delete": False},
    "data_disk": {"name_suffix": "-data", "type": "pd-balanced", "size_gb": 150, "device_name": "devbox-data",
                  "filesystem": "xfs", "mount_path": "/mnt/sandbox-data", "auto_delete": False},
    "bucket": {"name_prefix": "superserve-devbox-", "location": "us-central1", "storage_class": "STANDARD",
               "uniform_bucket_level_access": True, "public_access_prevention": "enforced",
               "soft_delete_retention_seconds": 0},
}
SECRETS = {"DEVBOX_SECRET_STRIPE_SECRET_KEY": "sk_test_example", "DEVBOX_SECRET_RECAPTCHA_API_KEY": "recaptcha-secret",
           "DEVBOX_CONFIG_STRIPE_PRO_MONTHLY_PRICE_ID": "price_example", "DEVBOX_CONFIG_BILLING_RESOURCE_CONFIG": "{\"cpu\": 1, \"memory_mib\": 1024}", "DEVBOX_CONFIG_RECAPTCHA_SITE_KEY": "site-key"}


class FakeCloud:
    """Stands in for gcloud, git ls-remote and the box behind IAP SSH."""

    def __init__(self, *, existing=(), branches=None, fail_script=None, instances=None, relabel_after=None,
                 decoys=(), fail_after_create=False, expire_on_script=None):
        self.commands, self.inputs = [], {}
        self.decoys, self.fail_after_create, self.expire_on_script = decoys, fail_after_create, expire_on_script
        self.disks, self.buckets, self.unlabelled = [], [], set()
        self.relabel_after_checkouts = None
        self.repo_scripts = list(contract.CREATE_SCRIPTS)
        self.existing = list(existing)
        self.branches = branches if branches is not None else {"main": SANDBOX_SHA, "feature": NEW_SHA}
        self.fail_script = fail_script
        self.instances = instances or {}
        self.metadata_writes, self.box_files, self.scripts, self.checkouts = [], {}, [], []
        self.relabel_after = relabel_after

    def __call__(self, args, input=None, **kwargs):
        self.commands.append(args)
        ok = lambda stdout="": subprocess.CompletedProcess(args, 0, stdout, "")
        fail = lambda code=1, stderr="failed": subprocess.CompletedProcess(args, code, "", stderr)
        if args[:2] == ["git", "check-ref-format"]:
            return ok() if re.fullmatch(r"[A-Za-z0-9/_.-]+", args[-1]) and ".." not in args[-1] else fail()
        if args[:2] == ["git", "init"] or (args[0] == "git" and "fetch" in args):
            return ok()
        if args[0] == "git" and "ls-tree" in args:
            return ok("".join(f"deploy/devbox/{script}\n" for script in self.repo_scripts))
        if args[:2] == ["git", "ls-remote"]:
            refs = {f"refs/heads/{branch}": sha for branch, sha in self.branches.items()}
            refs.update({ref: "f" * 40 for ref in self.decoys})
            lines = [f"{sha}\t{ref}\n" for ref, sha in sorted(refs.items())
                     if ref == args[-1] or ref.endswith("/" + args[-1])]
            return ok("".join(lines)) if lines else fail(2, "")
        assert args[0] == "gcloud", args
        verb = args[1:4]
        if verb == ["compute", "instances", "list"]:
            if "--format=json" in args:
                return ok(json.dumps(list(self.instances.values())))
            return ok("".join(f"{name}\tus-central1-a\n" for name in self.existing))
        if verb == ["compute", "disks", "list"]:
            if "--format=json" in args:
                return ok(json.dumps([{"name": name, "zone": "projects/p/zones/us-central1-a"}
                                      for name in self.disks if name not in self.unlabelled]))
            return ok()
        if verb == ["compute", "disks", "add-labels"]:
            self.unlabelled.discard(args[4])
            return ok()
        if verb == ["compute", "disks", "create"]:
            self.disks.append(args[4])
            return ok()
        if verb == ["storage", "buckets", "create"]:
            self.buckets.append(args[4])
            return ok()
        if verb == ["storage", "buckets", "describe"]:
            return ok(args[4]) if args[4] in self.buckets else fail(1, "ERROR: gs://x not found: 404.")
        if verb == ["compute", "instances", "create"]:
            labels = dict(pair.split("=") for pair in arg(args, "--labels").split(","))
            manifest = Path(arg(args, "--metadata-from-file").split("=", 1)[1]).read_text()
            self.instances[args[4]] = {"name": args[4], "zone": "projects/p/zones/us-central1-a", "labels": labels,
                                       "metadata": {"items": [{"key": "devbox-manifest", "value": manifest}]},
                                       "disks": [{"boot": True, "source": f"projects/p/zones/us-central1-a/disks/{args[4]}"},
                                                 {"source": f"projects/p/zones/us-central1-a/disks/{args[4]}-data"}]}
            self.disks.append(args[4])
            self.unlabelled.add(args[4])
            return fail(1, "operation timed out") if self.fail_after_create else ok()
        if verb == ["compute", "instances", "describe"]:
            instance = self.instances[args[4]]
            if (self.relabel_after is not None and len(self.metadata_writes) >= self.relabel_after) or \
                    (self.relabel_after_checkouts is not None and len(self.checkouts) >= self.relabel_after_checkouts):
                instance = {**instance, "labels": {**instance["labels"], "devbox-id": "ffffffffffff"}}
            return ok(json.dumps(instance))
        if verb == ["compute", "instances", "add-metadata"]:
            value = Path(arg(args, "--metadata-from-file").split("=", 1)[1]).read_text()
            self.metadata_writes.append(json.loads(value))
            self.instances[args[4]]["metadata"] = {"items": [{"key": "devbox-manifest", "value": value}]}
            return ok()
        if verb == ["compute", "ssh", args[3]]:
            return self.ssh(args, args[args.index("--command") + 1], input, ok, fail)
        return ok()

    def ssh(self, args, command, input, ok, fail):
        self.inputs[command] = input
        if command == "true":
            return ok()
        target = re.search(r"mv -f \S+ (/\S+?)'?$", unquote(command))
        if target and input is not None and "cat >" in unquote(command):
            self.box_files[target.group(1)] = input
            return ok()
        if "git -C" in unquote(command):
            self.checkouts.append(re.search(r"checkout -q --force --detach ([0-9a-f]{40})", unquote(command)).group(1))
            return ok()
        script = re.search(r"deploy/devbox/([a-z-]+\.sh)'? 2>&1", unquote(command))
        if script:
            self.scripts.append(script.group(1))
            if script.group(1) == self.expire_on_script:
                workflow.DEADLINE.end = 0
            return fail() if script.group(1) == self.fail_script else ok()
        if command.startswith("sudo cat"):
            path = shlex.split(command)[2]
            if path.endswith("runtime.json"):
                return ok(json.dumps({"firecracker": "1.10.1"}))
            if path.endswith("services.json"):
                return ok(json.dumps({"api": {"ready": True}}))
            return ok(self.box_files.get(path, ""))
        return ok()

    def gcloud(self, *verb):
        return [c for c in self.commands if c[0] == "gcloud" and c[1:1 + len(verb)] == list(verb)]


def arg(args, flag):
    return next(a.split("=", 1)[1] for a in args if a.startswith(flag + "="))


def unquote(command):
    return command.replace("'\"'\"'", "'")


class WorkflowCase(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.output, self.summary, self.profile = root / "output", root / "summary", root / "profile.json"
        self.output.write_text("")
        self.summary.write_text("")
        self.profile.write_text(json.dumps(PROFILE))
        patcher = patch.object(workflow.time, "sleep")
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(workflow.DEADLINE.stop)

    def env(self, **values):
        return {"GITHUB_REF": "refs/heads/main", "GITHUB_ACTOR": "AWille-AI", "GITHUB_OUTPUT": str(self.output),
                "GITHUB_STEP_SUMMARY": str(self.summary), **SECRETS, **values}

    def outputs(self):
        return dict(line.split("=", 1) for line in self.output.read_text().splitlines())

    def run_with(self, cloud, function, *args, **kwargs):
        with patch.object(workflow.subprocess, "run", side_effect=cloud), \
             patch("sys.stdout", new=open(os.devnull, "w")), patch("sys.stderr", new=open(os.devnull, "w")):
            return function(*args, **kwargs)


class PrepareTests(WorkflowCase):
    def test_prepare_create_generates_name_and_resolves_shas(self):
        cloud = FakeCloud(branches={"main": SANDBOX_SHA, "feature": NEW_SHA})
        self.run_with(cloud, workflow.prepare_create, self.env(INPUT_NAME="", INPUT_SANDBOX_BRANCH="feature",
                                                            INPUT_UI_BRANCH="", INPUT_LIFETIME_HOURS="48"),
                      profile_path=self.profile)
        outputs = self.outputs()
        self.assertRegex(outputs["name"], r"^awille-ai-dev-[0-9a-f]{6}$")
        self.assertEqual(outputs["owner"], "awille-ai")
        self.assertEqual(outputs["lifetime_hours"], "48")
        self.assertEqual((outputs["sandbox_branch"], outputs["sandbox_sha"]), ("feature", NEW_SHA))
        self.assertEqual((outputs["ui_branch"], outputs["ui_sha"]), ("main", SANDBOX_SHA))
        self.assertEqual([c[:2] for c in cloud.commands if c[0] != "git"], [])
        self.assertIn(NEW_SHA, next(c for c in cloud.commands if "fetch" in c))

    def test_prepare_create_refuses_revisions_without_the_box_scripts(self):
        cloud = FakeCloud()
        cloud.repo_scripts.remove("seed-base.sh")
        with self.assertRaisesRegex(workflow.StepError, "seed-base.sh"):
            self.run_with(cloud, workflow.prepare_create, self.env(), profile_path=self.profile)
        self.assertEqual(self.output.read_text(), "")
        with self.assertRaisesRegex(workflow.StepError, "devbox profile"):
            self.run_with(FakeCloud(), workflow.prepare_create, self.env(), profile_path=self.profile.with_name("x"))

    def test_branch_resolution_ignores_suffix_matches(self):
        cloud = FakeCloud(decoys=["refs/heads/evil/refs/heads/main"])
        self.assertEqual(self.run_with(cloud, workflow.resolve_branch, "sandbox", "main")["sha"], SANDBOX_SHA)
        cloud = FakeCloud(branches={}, decoys=["refs/heads/evil/refs/heads/main"])
        with self.assertRaisesRegex(workflow.StepError, "does not exist"):
            self.run_with(cloud, workflow.resolve_branch, "sandbox", "main")

    def test_prepare_create_rejects_bad_inputs(self):
        for values in ({"INPUT_SANDBOX_BRANCH": "missing"}, {"INPUT_UI_BRANCH": "a..b"}, {"INPUT_UI_BRANCH": "x`[y](z)"},
                       {"INPUT_LIFETIME_HOURS": "169"}, {"INPUT_NAME": "Bad_Name"},
                       {"GITHUB_REF": "refs/heads/feature"}):
            with self.subTest(values=values), self.assertRaises((workflow.StepError, contract.ContractError)):
                self.run_with(FakeCloud(), workflow.prepare_create, self.env(**values), profile_path=self.profile)

class CreateTests(WorkflowCase):
    def create_env(self, **values):
        return self.env(NAME="my-box", OWNER="awille-ai", LIFETIME_HOURS="24", SANDBOX_BRANCH="main",
                        SANDBOX_SHA=SANDBOX_SHA, UI_BRANCH="main", UI_SHA=UI_SHA, **values)

    def create(self, cloud, **values):
        return self.run_with(cloud, workflow.create, self.create_env(**values), profile_path=self.profile)

    def test_create_provisions_installs_and_records_ready(self):
        cloud = FakeCloud()
        self.assertEqual(self.create(cloud), 0)

        creates = [c[1:4] for c in cloud.commands if c[0] == "gcloud" and ("create" in c[1:4])]
        self.assertEqual(creates, [["compute", "disks", "create"], ["compute", "instances", "create"],
                                   ["storage", "buckets", "create"]])
        instance = cloud.gcloud("compute", "instances", "create")[0]
        self.assertEqual(instance[4], "devbox-my-box")
        self.assertEqual(cloud.gcloud("compute", "disks", "create")[0][4], "devbox-my-box-data")
        self.assertEqual(cloud.gcloud("compute", "disks", "add-labels")[0][4], "devbox-my-box")
        for flag in ("--no-address", "--no-boot-disk-auto-delete", "--enable-nested-virtualization",
                     "--disk=name=devbox-my-box-data,device-name=devbox-data,mode=rw,auto-delete=no",
                     "--subnet=superserve-devbox-usc1", "--tags=superserve-devbox",
                     "--metadata=block-project-ssh-keys=TRUE,enable-oslogin=TRUE"):
            self.assertIn(flag, instance)
        labelled = [arg(c, "--labels") for c in cloud.commands if any(a.startswith("--labels=") for a in c)]
        labelled += [arg(c, "--update-labels") for c in cloud.gcloud("storage", "buckets", "update")]
        self.assertEqual(len(labelled), 4)
        for labels in labelled:
            values = dict(pair.split("=") for pair in labels.split(","))
            self.assertEqual(set(values), set(contract.LABEL_KEYS))
            self.assertEqual(values["managed-by"], "superserve-devbox")
        devbox_id = dict(pair.split("=") for pair in labelled[0].split(","))["devbox-id"]
        self.assertIn(f"gs://superserve-devbox-{devbox_id}", cloud.gcloud("storage", "buckets", "create")[0])
        self.assertIn("--soft-delete-duration=0", cloud.gcloud("storage", "buckets", "create")[0])

        self.assertEqual(cloud.checkouts, [SANDBOX_SHA, UI_SHA])
        self.assertEqual(cloud.scripts, list(contract.CREATE_SCRIPTS))
        for args in cloud.gcloud("compute", "ssh"):
            self.assertIn("--tunnel-through-iap", args)
        final = cloud.metadata_writes[-1]
        self.assertEqual(final["state"], "ready")
        self.assertEqual(final["runtime"], {"firecracker": "1.10.1"})
        self.assertEqual(final["services"], {"api": {"ready": True}})
        self.assertEqual(final["revisions"], {"sandbox": {"branch": "main", "sha": SANDBOX_SHA},
                                              "ui": {"branch": "main", "sha": UI_SHA}})
        self.assertEqual(json.loads(cloud.box_files["/etc/superserve-devbox/manifest.json"]), final)
        summary = self.summary.read_text()
        self.assertIn("is ready", summary)
        self.assertIn(contract.forwarding_command("my-box"), summary)
        self.assertIn(workflow.destroy_command("my-box"), summary)

    def test_secrets_travel_over_stdin_only(self):
        cloud = FakeCloud()
        self.assertEqual(self.create(cloud), 0)
        secrets_file = cloud.box_files["/etc/superserve-devbox/secrets.env"]
        self.assertEqual(secrets_file, "RECAPTCHA_API_KEY=recaptcha-secret\nSTRIPE_SECRET_KEY=sk_test_example\n")
        devbox_env = cloud.box_files["/etc/superserve-devbox/devbox.env"]
        self.assertIn("STRIPE_PRO_MONTHLY_PRICE_ID=price_example\n", devbox_env)
        self.assertIn("""BILLING_RESOURCE_CONFIG='{"cpu": 1, "memory_mib": 1024}'\n""", devbox_env)
        self.assertIn(f"DEVBOX_SANDBOX_SHA={SANDBOX_SHA}\n", cloud.box_files["/etc/superserve-devbox/devbox.env"])
        command = next(c for c in cloud.inputs if "secrets.env" in c)
        self.assertIn("chmod 0600", command)
        for args in cloud.commands:
            self.assertFalse(any("sk_test_example" in a or "recaptcha-secret" in a for a in args), args)

    def test_unset_secret_fails_before_provisioning(self):
        cloud = FakeCloud()
        with self.assertRaises(workflow.StepError):
            self.create(cloud, DEVBOX_SECRET_STRIPE_SECRET_KEY="")
        self.assertEqual(cloud.commands, [])

    def test_name_collision_is_refused_without_offering_cleanup(self):
        cloud = FakeCloud(existing=["my-box"])
        with self.assertRaisesRegex(workflow.StepError, "already in use"):
            self.create(cloud)
        self.assertFalse([c for c in cloud.commands if "create" in c])
        self.assertIn("already in use", self.summary.read_text())
        self.assertNotIn("devbox-manage.yml", self.summary.read_text())
        self.assertFalse(cloud.gcloud("compute", "instances", "add-metadata"))

    def test_failed_install_marks_failed_and_reports_resources(self):
        cloud = FakeCloud(fail_script="install-api.sh")
        self.assertEqual(self.create(cloud), 1)
        self.assertEqual(cloud.scripts, ["host-bootstrap.sh", "services-up.sh", "install-api.sh"])
        self.assertEqual(cloud.metadata_writes[-1]["state"], "failed")
        summary = self.summary.read_text()
        self.assertNotIn("is ready", summary)
        for resource in ("disk `devbox-my-box-data`", "instance `devbox-my-box`", "disk `devbox-my-box`",
                         "bucket `superserve-devbox-"):
            self.assertIn(resource, summary)
        self.assertIn(workflow.destroy_command("my-box"), summary)

    def test_timed_out_instance_create_is_still_found_and_marked_failed(self):
        cloud = FakeCloud(fail_after_create=True)
        self.assertEqual(self.create(cloud), 1)
        self.assertEqual(cloud.metadata_writes[-1]["state"], "failed")
        summary = self.summary.read_text()
        self.assertIn("instance `devbox-my-box`", summary)
        self.assertIn("disk `devbox-my-box-data`", summary)
        self.assertIn("disk `devbox-my-box` ", summary)
        self.assertIn("devbox-my-box", cloud.unlabelled)
        self.assertNotIn("bucket `", summary)
        self.assertFalse([c for c in cloud.commands if any("name=" in a for a in c) and "list" in c
                          and any("devbox-id" in a for a in c)])

    def test_interrupted_create_is_marked_failed_by_the_backstop(self):
        attempt = Path(self.directory.name) / "attempt.json"
        cloud = FakeCloud(fail_script="install-api.sh")
        self.create(cloud, DEVBOX_ATTEMPT_FILE=str(attempt))
        self.assertTrue(json.loads(attempt.read_text())["reported"])
        writes = len(cloud.metadata_writes)
        self.run_with(cloud, workflow.finish_create, self.create_env(DEVBOX_ATTEMPT_FILE=str(attempt)))
        self.assertEqual(len(cloud.metadata_writes), writes)

        devbox_id = json.loads(attempt.read_text())["devbox_id"]
        instance = cloud.instances["devbox-my-box"]
        manifest = json.loads(instance["metadata"]["items"][0]["value"])
        instance["metadata"]["items"][0]["value"] = json.dumps({**manifest, "state": "creating"})
        attempt.write_text(json.dumps({"name": "my-box", "devbox_id": devbox_id, "reported": False}))
        self.run_with(cloud, workflow.finish_create, self.create_env(DEVBOX_ATTEMPT_FILE=str(attempt)))
        self.assertEqual(cloud.metadata_writes[-1]["state"], "failed")
        self.assertIn("cancelled or timed out", self.summary.read_text())

    def test_deadline_still_leaves_the_box_marked_failed(self):
        attempt = Path(self.directory.name) / "attempt.json"
        cloud = FakeCloud(expire_on_script="install-api.sh")
        workflow.DEADLINE.start(3600)
        self.assertEqual(self.create(cloud, DEVBOX_ATTEMPT_FILE=str(attempt)), 1)
        self.assertEqual(cloud.scripts, ["host-bootstrap.sh", "services-up.sh", "install-api.sh"])
        self.assertEqual(cloud.metadata_writes[-1]["state"], "failed")
        self.assertIn("Ran out of time", self.summary.read_text())
        self.assertTrue(json.loads(attempt.read_text())["reported"])

    def test_box_replaced_during_a_checkout_is_not_touched_again(self):
        cloud = FakeCloud()
        cloud.relabel_after_checkouts = 1
        self.assertEqual(self.create(cloud), 1)
        self.assertEqual(cloud.checkouts, [SANDBOX_SHA])
        self.assertEqual(cloud.scripts, [])
        self.assertFalse([path for path in cloud.box_files if path.endswith(".env")])
        self.assertIn("devbox-id changed", self.summary.read_text())

    def test_replaced_box_is_never_written(self):
        cloud = FakeCloud(relabel_after=0)
        self.assertEqual(self.create(cloud), 1)
        self.assertEqual(cloud.scripts, [])
        self.assertEqual(cloud.metadata_writes, [])
        self.assertIn("devbox-id changed", self.summary.read_text())

    def test_profile_must_agree_with_the_contract(self):
        for profile in ({**PROFILE, "labels": {**PROFILE["labels"], "sandbox_role": "vmd"}},
                        {**PROFILE, "resource_name_prefix": "box-"},
                        {**PROFILE, "project": "other-project"},
                        {**PROFILE, "instance": {**PROFILE["instance"], "external_ip": True}},
                        {**PROFILE, "instance": {**PROFILE["instance"], "metadata": {}}},
                        {**PROFILE, "data_disk": {**PROFILE["data_disk"], "auto_delete": True}},
                        {key: value for key, value in PROFILE.items() if key != "bucket"}):
            self.profile.write_text(json.dumps(profile))
            cloud = FakeCloud()
            with self.subTest(profile=profile), self.assertRaises(workflow.StepError):
                self.create(cloud)
            self.assertEqual(cloud.commands, [])


if __name__ == "__main__":
    unittest.main()
