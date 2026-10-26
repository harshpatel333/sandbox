import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("deploy_vmd", Path(__file__).with_name("deploy-vmd.py"))
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)


class DiscoveryDiagnosticsTest(unittest.TestCase):
    def test_failure_reports_error_and_preserves_exit(self):
        error = subprocess.CalledProcessError(7, ["gcloud"], stderr="permission denied: compute.instances.list")
        output = io.StringIO()
        with patch.object(deploy.subprocess, "run", side_effect=error), contextlib.redirect_stderr(output):
            with self.assertRaises(SystemExit) as raised:
                deploy.discover_vmd_instances("example-project", "component=vmd")
        self.assertEqual(raised.exception.code, 7)
        self.assertIn("permission denied: compute.instances.list", output.getvalue())

    def test_credentials_and_control_characters_are_not_reported(self):
        with patch.dict(os.environ, {"INTERNAL_API_TOKEN": "example-sensitive-value"}):
            result = deploy.discovery_error("example-sensitive-value postgresql://user:pass@db.example Bearer token-value\x00")
        for value in ("example-sensitive-value", "user:pass", "token-value", "\x00"):
            self.assertNotIn(value, result)
        self.assertLessEqual(len(deploy.discovery_error("x" * 9000)), 4000)

    def test_diagnostic_only_calls_staging_discovery(self):
        result = subprocess.CompletedProcess([], 0, "example-vmd,us-central1-a\n", "")
        with patch.object(deploy.subprocess, "run", return_value=result) as run, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.diagnose_staging_discovery(), 0)
        run.assert_called_once_with(
            ["gcloud", "compute", "instances", "list", "--project=rayai-dev",
             "--filter=labels.component=vmd AND status=RUNNING", "--format=csv[no-heading](name,zone)"],
            capture_output=True, text=True, check=True,
        )


if __name__ == "__main__":
    unittest.main()
