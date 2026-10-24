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

            def wait(self, timeout):
                events.append('reap')
                return 0
        events = []
        with patch.object(runner, 'await_exit', side_effect=[subprocess.TimeoutExpired('synthetic', 90), 0]), patch.object(runner.os, 'killpg', side_effect=lambda pid, sig: events.append((pid, sig))):
            with self.assertRaises(subprocess.TimeoutExpired):
                runner.invoke(['unused'], run=lambda *args, **kwargs: Child())
        self.assertEqual(events, [(123456, runner.signal.SIGTERM), (123456, runner.signal.SIGKILL), 'reap'])

    def test_real_child_remains_unreaped_through_signals(self):
        runner = module('run_once')
        states = []
        actual_kill = runner.os.killpg

        def checked_kill(pid, sig):
            state = runner.os.waitid(runner.os.P_PID, pid, runner.os.WEXITED | runner.os.WNOHANG | runner.os.WNOWAIT)
            states.append(state is not None)
            actual_kill(pid, sig)

        with patch.object(runner.os, 'killpg', side_effect=checked_kill):
            self.assertEqual(runner.invoke(['/bin/true']), 0)
        self.assertEqual(states, [True, True])

    def test_proxy_has_no_key_registration(self):
        proxy = module('iap_proxy')
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / 'gcloud'
            binary.write_bytes(b'synthetic')
            plan = Path(directory) / 'plan.json'
            plan.write_text(json.dumps({'gcloud': str(binary), 'gcloud_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'instance': 'example-instance', 'project': 'example-project', 'zone': 'example-zone'}))
            plan.chmod(0o600)
            digest = hashlib.sha256(plan.read_bytes()).hexdigest()
            argv = proxy.command(plan, digest)
            self.assertEqual(argv[1:5], ['compute', 'start-iap-tunnel', 'example-instance', '22'])
            self.assertIn('--listen-on-stdin', argv)
            self.assertNotIn('ssh', argv)
            changed = json.loads(plan.read_bytes())
            changed["instance"] = "example-other"
            plan.write_text(json.dumps(changed))
            with self.assertRaises(ValueError):
                proxy.command(plan, digest)
            plan.chmod(0o644)
            with self.assertRaises(ValueError):
                proxy.command(plan, digest)


if __name__ == '__main__':
    unittest.main()
