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

    def test_inherited_agent_and_x11_forwarding_are_disabled_without_connection(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            config = Path(args.ssh_config)
            config.write_text('Host rc-billing-readonly\n HostName example.invalid\n ForwardAgent yes\n ForwardX11 yes\n')
            args.ssh_config_sha256 = hashlib.sha256(config.read_bytes()).hexdigest()
            completed = subprocess.CompletedProcess([], 1, b'{"status":"PARTIAL_OR_FAILED_STATIC_OBSERVATION"}', b'')
            with patch.object(caller.subprocess, 'run', return_value=completed) as run:
                caller.execute(args)
                command = run.call_args.args[0]
            command[command.index('-F') + 1] = str(config)
            effective = subprocess.run([command[0], '-G', *command[1:-1]], check=True, capture_output=True, text=True)
            settings = dict(line.split(' ', 1) for line in effective.stdout.splitlines() if ' ' in line)
            self.assertEqual(settings['forwardagent'], 'no')
            self.assertEqual(settings['forwardx11'], 'no')
            self.assertEqual(settings['clearallforwardings'], 'yes')

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

    def test_validated_config_snapshot_is_sealed_despite_source_change(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            expected = Path(args.ssh_config).read_bytes()
            def run(command, **kwargs):
                Path(args.ssh_config).write_text('changed after validation')
                descriptor = kwargs['pass_fds'][0]
                import os
                import errno
                os.lseek(descriptor, 0, os.SEEK_SET)
                self.assertEqual(os.read(descriptor, 131072), expected)
                with self.assertRaises(OSError) as raised:
                    os.write(descriptor, b'changed')
                self.assertEqual(raised.exception.errno, errno.EPERM)
                self.assertEqual(command[command.index('-F') + 1], '/proc/self/fd/' + str(descriptor))
                return subprocess.CompletedProcess([], 1, b'{"status":"PARTIAL_OR_FAILED_STATIC_OBSERVATION"}', b'')
            with patch.object(caller.subprocess, 'run', side_effect=run):
                self.assertEqual(caller.execute(args)['status'], 'STATIC_OBSERVATION_RECORDED')

    def test_empty_digest_refuses_before_call(self):
        for field in ('ssh_config_sha256', 'pins_sha256'):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as directory:
                args = self.setup_args(Path(directory))
                setattr(args, field, '')
                with patch.object(caller.subprocess, 'run') as run, self.assertRaises(ValueError):
                    caller.execute(args)
                run.assert_not_called()
                self.assertFalse(Path(args.once_file).exists())

    def test_parser_errors_do_not_echo_unknown_values(self):
        script = Path(__file__).with_name('pricing-static-observer-ssh.py')
        completed = subprocess.run(['python3', str(script), '--unknown', 'NONSECRET_SENTINEL'], capture_output=True, text=True)
        self.assertNotEqual(completed.returncode, 0)
        self.assertEqual(completed.stderr, '')
        self.assertNotIn('NONSECRET_SENTINEL', completed.stdout)

    def test_output_write_failure_does_not_claim_no_remote_call(self):
        import io
        with tempfile.TemporaryDirectory() as directory:
            args = self.setup_args(Path(directory))
            argv = ['caller']
            for name, value in vars(args).items():
                argv.extend(['--' + name.replace('_', '-'), value])
            completed = subprocess.CompletedProcess([], 1, b'{"status":"PARTIAL_OR_FAILED_STATIC_OBSERVATION"}', b'')
            with patch.object(caller.subprocess, 'run', return_value=completed) as run, patch.object(caller.json, 'dump', side_effect=OSError('PRIVATE_SENTINEL')), patch('sys.argv', argv), patch('sys.stdout', new_callable=io.StringIO) as stdout:
                self.assertEqual(caller.main(), 1)
                run.assert_called_once()
            self.assertIn('EXECUTION_STATE_UNAVAILABLE_NO_RETRY', stdout.getvalue())
            self.assertNotIn('NO_REMOTE_CALL', stdout.getvalue())
            self.assertNotIn('PRIVATE_SENTINEL', stdout.getvalue())

    def test_complete_requires_bound_schema_and_preserved_prior(self):
        pins = {'backend-sha': 'a' * 40, 'console-sha': 'b' * 40, 'binary-sha256': 'c' * 64, 'schema-sha256': 'd' * 64, 'epoch': 'epoch'}
        runtime = {'backend_sha': pins['backend-sha'], 'console_sha': pins['console-sha'], 'binary_sha256': pins['binary-sha256'], 'schema_receipt_sha256': pins['schema-sha256'], 'epoch': 'epoch', 'pid': 2, 'console_container_id': 'e' * 64, 'pricing_version': 'v2', 'entry_open': True}
        compiled = dict.fromkeys(('recaptcha_script_code', 'turnstile_script_code', 'fingerprint_client_code', 'recaptcha_candidate_key_literal', 'turnstile_candidate_key_literal', 'fingerprint_candidate_key_literal'), False)
        env = dict.fromkeys(('recaptcha_api_key', 'recaptcha_project_id', 'recaptcha_site_key', 'turnstile_site_key', 'fingerprint_api_key'), False)
        prior = {'source_run': 40122720783, 'original_run': 40121793821, 'observed_at': '2026-10-24T18:53:57+00:00', 'counts': dict.fromkeys(('auth_users', 'confirmed_auth_users', 'profiles', 'owner_teams', 'current_assignments'), 0), 'mailpit_counts': {'matching_recipient_messages': 0, 'confirmation_subject_messages': 0, 'counts_complete': True}}
        value = {'status': 'STATIC_OBSERVATION_COMPLETE', 'runtime_before': runtime, 'runtime_after': dict(runtime), 'static_presence': {'compiled_asset_presence': compiled, 'running_server_env_presence': env}, 'prior_observations': prior}
        self.assertEqual(caller.sanitized(value, pins)['prior_observations']['counts']['auth_users'], 0)
        value['prior_observations'] = None
        with self.assertRaises(ValueError):
            caller.sanitized(value, pins)
        value['prior_observations'] = prior
        value['runtime_after']['schema_receipt_sha256'] = 'f' * 64
        with self.assertRaises(ValueError):
            caller.sanitized(value, pins)

    def test_no_false_completion_and_unrecognized_failure_text(self):
        for value in ({'status': 'STATIC_OBSERVATION_COMPLETE'}, {'status': 'PARTIAL_OR_FAILED_STATIC_OBSERVATION', 'primary_failure': {'stage': 'secret', 'exception_class': 'OSError'}}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                caller.sanitized(value, {})


if __name__ == '__main__':
    unittest.main()
