import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import json
import hashlib
import subprocess

ROOT = Path(__file__).resolve().parent


def module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + '.py'))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


class DesignTests(unittest.TestCase):
    def test_disabled_runner_never_launches(self):
        runner = module('run_once')
        with patch.object(runner.subprocess, 'Popen') as launch, patch.object(runner.sys, 'argv', ['runner', 'unused']):
            self.assertEqual(runner.main(), 1)
            launch.assert_not_called()

    def test_timeout_cleans_only_child_group(self):
        runner = module('run_once')
        class Child:
            pid = 123456
            calls = 0

            def wait(self, timeout):
                self.calls += 1
                if self.calls == 1:
                    raise subprocess.TimeoutExpired('synthetic', timeout)
                return 0
        with patch.object(runner.os, 'killpg') as kill:
            with self.assertRaises(subprocess.TimeoutExpired):
                runner.invoke(['unused'], run=lambda *args, **kwargs: Child())
            self.assertEqual([call.args[0] for call in kill.call_args_list], [123456, 123456])

    def test_proxy_has_no_key_registration(self):
        proxy = module('iap_proxy')
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / 'gcloud'
            binary.write_bytes(b'synthetic')
            plan = Path(directory) / 'plan.json'
            plan.write_text(json.dumps({'gcloud': str(binary), 'gcloud_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'instance': 'example-instance', 'project': 'example-project', 'zone': 'example-zone'}))
            plan.chmod(0o600)
            argv = proxy.command(plan)
            self.assertEqual(argv[1:5], ['compute', 'start-iap-tunnel', 'example-instance', '22'])
            self.assertIn('--listen-on-stdin', argv)
            self.assertNotIn('ssh', argv)
            plan.chmod(0o644)
            with self.assertRaises(ValueError):
                proxy.command(plan)


if __name__ == '__main__':
    unittest.main()
