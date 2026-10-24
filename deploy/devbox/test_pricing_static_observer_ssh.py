import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('caller', Path(__file__).with_name('pricing-static-observer-ssh.py'))
caller = importlib.util.module_from_spec(spec)
spec.loader.exec_module(caller)


class CallerTest(unittest.TestCase):
    def setup_args(self, root):
        config = root / 'ssh-config'
        config.write_text('Host rc-billing-readonly\n HostName example.invalid\n')
        config.chmod(0o600)
        pins = {name: 'placeholder' for name in caller.ARGUMENTS}
        pins.update({'devbox-name': 'awille-ai-dev-billing', 'devbox-id': '29baf003ad05', 'backend-sha': '4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b', 'console-sha': '587039f5e5bd9994e4e2f52a7c6a2e5daf452984'})
        source = root / 'pins.json'
        source.write_text(json.dumps(pins))
        source.chmod(0o600)
        return argparse.Namespace(ssh_config=str(config), ssh_config_sha256=hashlib.sha256(config.read_bytes()).hexdigest(), pins=str(source), pins_sha256=hashlib.sha256(source.read_bytes()).hexdigest(), once_file=str(root / 'once'), output=str(root / 'out'))

    def test_exactly_one_streamed_call_without_target_staging_or_privilege_options(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            payload = {'status': 'PARTIAL_OR_FAILED_STATIC_OBSERVATION', 'primary_failure': {'stage': 'static.process_env', 'exception_class': 'PermissionError', 'errno': 13, 'status': 1}, 'raw_secret': 'sentinel'}
            completed = subprocess.CompletedProcess([], 1, json.dumps(payload).encode(), b'secret stderr')
            with patch.object(caller.subprocess, 'run', return_value=completed) as run:
                result = caller.execute(args)
                run.assert_called_once()
                command = run.call_args.args[0]
                self.assertEqual(command[0], '/usr/bin/ssh')
                self.assertIn('BatchMode=yes', command)
                self.assertIn('StrictHostKeyChecking=yes', command)
                self.assertNotIn('sudo', command[-1])
                self.assertNotIn('git ', command[-1])
                self.assertNotIn('scp', command)
                streamed = json.loads(run.call_args.kwargs['input'])
                import base64
                self.assertEqual(hashlib.sha256(base64.b64decode(streamed['source'])).hexdigest(), caller.LEAF_SHA256)
                self.assertEqual(result['actual_static_result']['primary_failure']['errno'], 13)
                self.assertNotIn('sentinel', json.dumps(result))
                self.assertNotIn('secret stderr', json.dumps(result))
                args.output += '-second'
                second = caller.execute(args)
                self.assertEqual(second['status'], 'CALLER_FAILED_OR_AMBIGUOUS_NO_RETRY')
                run.assert_called_once()

    def test_timeout_is_sanitized_and_consumes_one_execution_marker(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            with patch.object(caller.subprocess, 'run', side_effect=subprocess.TimeoutExpired('secret command', 80, stderr=b'secret')):
                result = caller.execute(args)
            self.assertEqual(result['failure']['exception_class'], 'TimeoutError')
            self.assertTrue(Path(args.once_file).exists())
            self.assertNotIn('secret', json.dumps(result))

    def test_config_hash_mismatch_refuses_before_call_or_marker(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            args.ssh_config_sha256 = '0' * 64
            with patch.object(caller.subprocess, 'run') as run, self.assertRaises(ValueError):
                caller.execute(args)
            run.assert_not_called()
            self.assertFalse(Path(args.once_file).exists())

    def test_no_false_completion_and_unrecognized_failure_text(self):
        for value in ({'status': 'STATIC_OBSERVATION_COMPLETE'}, {'status': 'PARTIAL_OR_FAILED_STATIC_OBSERVATION', 'primary_failure': {'stage': 'secret', 'exception_class': 'OSError'}}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                caller.sanitized(value, {})


if __name__ == '__main__':
    unittest.main()
