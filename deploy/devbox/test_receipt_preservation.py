import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / ".github/workflows/scripts"))
import devbox_workflow as workflow


class ReceiptPreservationTest(unittest.TestCase):
    def clear(self, mode, root):
        record = root / "api-deploy.json"
        if mode == "workflow":
            box = workflow.Box("example", "us-central1-a")
            def ssh(command):
                return subprocess.run(shlex.split(command)[1:], check=True, capture_output=True)
            with patch.object(box, "guard"), patch.object(box, "ssh", side_effect=ssh), patch.object(workflow, "API_DEPLOY_FILE", str(record)):
                box.remove(str(record))
        else:
            env = dict(os.environ, DEVBOX_STATE_DIR=str(root))
            subprocess.run(["bash", "-euc", 'source "$1"; clear_api_deploy_record', "test", str(ROOT / "deploy/devbox/services/common.sh")], env=env, check=True, capture_output=True)

    def test_each_clear_preserves_exact_prior_receipts(self):
        for mode in ("workflow", "installer"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                record = root / "api-deploy.json"
                contents = [b'{"sha":"old","migrations":"applied"}\n', b'{"sha":"next","migrations":"none"}\n']
                for content in contents:
                    record.write_bytes(content)
                    record.chmod(0o600)
                    self.clear(mode, root)
                    self.assertFalse(record.exists())
                archive = root / "api-deploy-history"
                self.assertEqual(sorted(p.read_bytes() for p in archive.glob("*/api-deploy.json")), sorted(contents))
                self.assertEqual(archive.stat().st_mode & 0o777, 0o700)
                self.clear(mode, root)
                self.assertEqual(len(list(archive.glob("*/api-deploy.json"))), 2)

    def test_archive_failure_keeps_current_receipt(self):
        for mode in ("workflow", "installer"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                record = root / "api-deploy.json"
                record.write_bytes(b"original")
                (root / "api-deploy-history").write_bytes(b"not a directory")
                with self.assertRaises(subprocess.CalledProcessError):
                    self.clear(mode, root)
                self.assertEqual(record.read_bytes(), b"original")


if __name__ == "__main__":
    unittest.main()
