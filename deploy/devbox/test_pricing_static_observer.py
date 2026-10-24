import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location('pricing_static_observer', Path(__file__).with_name('pricing-static-observer.py'))
OBSERVER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OBSERVER)
SNAPSHOT = {'backend_sha': 'a' * 40, 'console_sha': 'b' * 40, 'epoch': 'example-epoch'}
PRIOR = {'counts': {'auth_users': 0}, 'mailpit_counts': {'matching_recipient_messages': 0}, 'prior_inspection_complete': False}


class FakeObserver(OBSERVER.Observer):
    def __init__(self, primary=None, after=None):
        super().__init__(None)
        self.primary = primary
        self.after = after
        self.calls = []

    def prior(self):
        return PRIOR

    def validate_target(self):
        self.target_validated = True

    def runtime(self, stage):
        self.calls.append(stage)
        if stage == 'runtime.after' and self.after:
            raise self.after
        return SNAPSHOT

    def static(self):
        self.calls.append('static')
        if self.primary:
            raise self.primary
        return {'compiled_asset_presence': {'recaptcha_script_code': False}}


class StaticObserverTest(unittest.TestCase):
    def test_static_failure_keeps_counts_unknown_and_captures_after(self):
        failure = OBSERVER.Failure(OBSERVER.classified('static.process_env', PermissionError(13, 'PRIVATE_ENV_VALUE')))
        observer = FakeObserver(failure)
        result = observer.observe()
        self.assertEqual(result['prior_observations'], PRIOR)
        self.assertIsNone(result['static_presence'])
        self.assertEqual(result['primary_failure']['stage'], 'static.process_env')
        self.assertEqual(result['primary_failure']['errno'], 13)
        self.assertTrue(result['runtime_unchanged'])
        self.assertEqual(observer.calls, ['runtime.before', 'static', 'runtime.after'])
        self.assertNotIn('PRIVATE_ENV_VALUE', json.dumps(result))
        self.assertEqual(result['status'], 'PARTIAL_OR_FAILED_STATIC_OBSERVATION')

    def test_after_failure_never_replaces_original_failure(self):
        observer = FakeObserver(OBSERVER.Failure(OBSERVER.classified('static.asset_directory', FileNotFoundError(2, 'PRIVATE_PATH'))), subprocess.TimeoutExpired(['SECRET_ARG'], 20))
        result = observer.observe()
        self.assertEqual(result['primary_failure']['stage'], 'static.asset_directory')
        self.assertEqual(result['after_failure']['stage'], 'runtime.after')
        self.assertEqual(result['after_failure']['exception_class'], 'TimeoutError')
        self.assertIsNone(result['runtime_after'])
        self.assertIsNone(result['runtime_unchanged'])
        self.assertNotIn('SECRET', json.dumps(result))
        self.assertNotIn('PRIVATE', json.dumps(result))

    def test_no_false_success_if_after_fails(self):
        result = FakeObserver(after=PermissionError(13, 'private')).observe()
        self.assertIsNotNone(result['static_presence'])
        self.assertIsNone(result['primary_failure'])
        self.assertEqual(result['status'], 'PARTIAL_OR_FAILED_STATIC_OBSERVATION')

    def test_target_guard_failure_does_not_probe_target(self):
        observer = FakeObserver()
        observer.validate_target = mock.Mock(side_effect=ValueError('PRIVATE_TARGET'))
        result = observer.observe()
        self.assertEqual(observer.calls, [])
        self.assertEqual(result['primary_failure']['stage'], 'input.target')
        self.assertIsNone(result['runtime_after'])

    def test_complete_static_is_not_signup_or_sdk_pass(self):
        result = FakeObserver().observe()
        self.assertEqual(result['status'], 'STATIC_OBSERVATION_COMPLETE')
        self.assertEqual(result['root_cause'], 'NOT_ESTABLISHED')
        self.assertEqual(result['sdk_readiness'], 'UNAVAILABLE_NO_BROWSER_OR_SDK_PROBE')
        self.assertFalse(result['counts_requeried'])

    def test_raw_subprocess_failure_is_sanitized(self):
        observer = OBSERVER.Observer(None)
        observer.config = {}
        process = subprocess.CompletedProcess(['secret'], 127, '', 'PRIVATE_STDERR_TOKEN')
        with mock.patch.object(OBSERVER.subprocess, 'run', return_value=process):
            with self.assertRaises(OBSERVER.Failure) as caught:
                observer.static()
        self.assertEqual(caught.exception.evidence['stage'], 'static.launch')
        self.assertEqual(caught.exception.evidence['status'], 127)
        self.assertNotIn('PRIVATE_STDERR_TOKEN', str(caught.exception.evidence))

    def test_exact_node_stage_codes_and_unknowns_without_real_filesystem(self):
        harness = r'''
let captured='';
const fs={readFileSync:(f)=>{if(f===0)return '{}';if(f==='/proc/1/environ'){const e=new Error('SECRET_ENV');e.code='EACCES';throw e;}throw new Error('unexpected');}};
new Function('require','console','process',%s)((name)=>name==='node:fs'?fs:require(name),console,{stdout:{write:v=>captured=v}});
console.log(captured);
''' % json.dumps(OBSERVER.STATIC_NODE)
        process = subprocess.run(['node', '-e', harness], capture_output=True, text=True, check=True)
        result = json.loads(process.stdout)
        self.assertFalse(result['ok'])
        self.assertEqual(result['failure']['stage'], 'static.process_env')
        self.assertEqual(result['failure']['exception_class'], 'PermissionError')
        self.assertEqual(result['failure']['errno'], 13)
        self.assertNotIn('SECRET_ENV', process.stdout)
        self.assertNotIn('compiled_asset_presence', result)


    def test_argument_failure_never_echoes_supplied_values(self):
        result = subprocess.run(['python3', str(Path(OBSERVER.__file__)), '--PRIVATE_ARGUMENT=PRIVATE_VALUE'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stderr, '')
        self.assertNotIn('PRIVATE', result.stdout)
        self.assertEqual(json.loads(result.stdout)['primary_failure']['stage'], 'input.args')

    def test_runtime_child_failure_is_sanitized(self):
        observer = OBSERVER.Observer(None)
        observer.runtime_inner = mock.Mock(side_effect=PermissionError(13, 'PRIVATE_VALUE'))
        with self.assertRaises(OBSERVER.Failure) as caught:
            observer.runtime('runtime.after')
        self.assertEqual(caught.exception.evidence['errno'], 13)
        self.assertNotIn('PRIVATE', str(caught.exception.evidence))

    def test_exact_node_wrapper_bounds_child_and_hides_stderr(self):
        helper = "process.stderr.write('PRIVATE_VALUE');while(true){}"
        wrapper = OBSERVER.STATIC_WRAPPER.replace('timeout:15000', 'timeout:100')
        result = subprocess.run(['node', '-e', wrapper, helper], input='{}', capture_output=True, text=True, timeout=3)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stderr, '')
        failure = json.loads(result.stdout)['failure']
        self.assertEqual(failure['exception_class'], 'TimeoutError')
        self.assertNotIn('PRIVATE', result.stdout)

    def test_static_success_whitelists_fields_and_preserves_unknown_literals(self):
        observer = OBSERVER.Observer(None)
        observer.config = {}
        compiled = {name: False for name in ('recaptcha_script_code', 'turnstile_script_code', 'fingerprint_client_code')}
        compiled.update({name: None for name in ('recaptcha_candidate_key_literal', 'turnstile_candidate_key_literal', 'fingerprint_candidate_key_literal')})
        env = {name: False for name in ('recaptcha_api_key', 'recaptcha_project_id', 'recaptcha_site_key', 'turnstile_site_key', 'fingerprint_api_key')}
        payload = {'ok': True, 'compiled_asset_presence': compiled, 'running_server_env_presence': env, 'unexpected_secret': 'PRIVATE_TOKEN'}
        process = subprocess.CompletedProcess(['private'], 0, json.dumps(payload), 'PRIVATE_STDERR')
        with mock.patch.object(OBSERVER.subprocess, 'run', return_value=process):
            result = observer.static()
        self.assertIsNone(result['compiled_asset_presence']['recaptcha_candidate_key_literal'])
        self.assertNotIn('PRIVATE', json.dumps(result))
        self.assertTrue(result['absence_does_not_prove_buildtime_absence'])


if __name__ == '__main__':
    unittest.main()
