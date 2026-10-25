import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('install-ui.sh')

LOCAL_SECRETS = {
    'SERVICE_ROLE_KEY': 'service-role-secret',
    'INTERNAL_API_TOKEN': 'internal-secret',
    'OPERATOR_API_TOKEN': 'operator-secret',
    'OPERATOR_API_TOKEN': 'operator-secret',
    'PROMOTION_CAPTURE_TOKEN': 'capture-secret',
    'PROMOTION_ACCOUNT_TOKEN': 'account-secret',
    'POSTGRES_PASSWORD': 'postgres-secret',
}
PROMOTION_KEY = '-----BEGIN PRIVATE KEY-----\nprivate-key-secret\n-----END PRIVATE KEY-----\n'
CI_SECRETS = {'RECAPTCHA_API_KEY': 'recaptcha-secret'}


class InstallUiDryRunTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        (root / 'etc').mkdir()
        (root / 'etc' / 'devbox.env').write_text('RECAPTCHA_SITE_KEY=site-key\nRECAPTCHA_PROJECT_ID=recaptcha-project\n')
        (root / 'etc' / 'secrets.env').write_text(''.join(f'{k}={v}\n' for k, v in CI_SECRETS.items()))
        (root / 'src' / 'apps' / 'console').mkdir(parents=True)
        (root / 'src' / 'apps' / 'console' / 'package.json').write_text('{}')
        self.etc = root / 'etc'
        self.secrets = root / 'data'
        self.secrets.mkdir()
        (self.secrets / 'promotion-account-key.pem').write_text(PROMOTION_KEY)
        self.env = {
            'PATH': os.environ['PATH'],
            'DEVBOX_CONFIG_DIR': str(root / 'etc'),
            'DEVBOX_UI_SRC': str(root / 'src'),
            'DEVBOX_DATA_DIR': str(self.secrets),
        }

    def run_script(self, local):
        (self.etc / 'local-secrets.env').write_text(''.join(f'{k}={v}\n' for k, v in local.items()))
        return subprocess.run([str(SCRIPT), '--dry-run'], env=self.env, capture_output=True, text=True)

    def test_build_args_carry_public_values_and_no_server_secret(self):
        result = self.run_script({'ANON_KEY': 'anon-key', **LOCAL_SECRETS})

        self.assertEqual(result.returncode, 0, result.stderr)
        build_args = [line for line in result.stdout.splitlines() if line.startswith('build-arg ')]
        self.assertIn('build-arg NEXT_PUBLIC_SUPABASE_ANON_KEY=anon-key', build_args)
        self.assertIn('build-arg NEXT_PUBLIC_RECAPTCHA_SITE_KEY=site-key', build_args)
        self.assertIn('build-arg NEXT_PUBLIC_SANDBOX_HOST=sandbox.localhost', build_args)
        for secret in [*LOCAL_SECRETS.values(), 'private-key-secret', *CI_SECRETS.values()]:
            self.assertNotIn(secret, result.stdout)
        self.assertFalse((self.secrets / 'console-secrets.env').exists())

    def test_shared_values_are_read_verbatim_without_echoing_them(self):
        result = self.run_script({'ANON_KEY': "an$on key's", **LOCAL_SECRETS, 'DATABASE_URL': 'postgresql://postgres:p w@127.0.0.1:54322/postgres'})

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("build-arg NEXT_PUBLIC_SUPABASE_ANON_KEY=an$on key's", result.stdout.splitlines())
        self.assertEqual(result.stderr, '')

    def test_calendly_values_are_runtime_only_and_not_printed(self):
        values = {'CALENDLY_API_TOKEN': 'private-calendar-token', 'CALENDLY_ORGANIZATION_URI': 'https://api.calendly.com/organizations/example', 'CALENDLY_ENTERPRISE_EVENT_TYPE_URI': 'https://api.calendly.com/event_types/example'}
        with (self.etc / 'secrets.env').open('a') as stream:
            stream.write(''.join(f'{key}={value}\n' for key, value in values.items()))
        result = self.run_script({'ANON_KEY': 'anon-key', **LOCAL_SECRETS})
        self.assertEqual(result.returncode, 0, result.stderr)
        for key, value in values.items():
            self.assertIn('runtime ' + key + '=…', result.stdout)
            self.assertNotIn(value, result.stdout + result.stderr)
            self.assertNotIn('build-arg ' + key, result.stdout)

    def test_configured_meeting_requires_existing_secret_before_mutation(self):
        source = SCRIPT.read_text()
        functions = source[source.index('die() {'):source.index('require_file "$DEVBOX_ENV"')]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'console-secrets.env'
            original = b'GOOGLE_SIGNUP_PROOF_SECRET=' + b'x' * 32 + b'\n'
            for content, success in ((None, False), (b'GOOGLE_SIGNUP_PROOF_SECRET=short\n', False), (original, True)):
                if content is not None:
                    path.write_bytes(content)
                result = subprocess.run(['bash', '-c', 'set -euo pipefail\n' + functions + '\nCONSOLE_SECRETS="$1"\nRUNTIME_VARS=(CALENDLY_API_TOKEN=token CALENDLY_ORGANIZATION_URI=org CALENDLY_ENTERPRISE_EVENT_TYPE_URI=event)\nrequire_existing_meeting_secret', '_', str(path)], capture_output=True, text=True)
                self.assertEqual(result.returncode == 0, success)
                if content is not None:
                    self.assertEqual(path.read_bytes(), content)
                else:
                    self.assertFalse(path.exists())
            self.assertLess(source.index('\nrequire_existing_meeting_secret\n'), source.index('\nsystemctl start superserve-vmd\n'))

    def test_missing_shared_key_fails_with_its_name(self):
        local = {'ANON_KEY': 'anon-key', **LOCAL_SECRETS}
        del local['SERVICE_ROLE_KEY']

        result = self.run_script(local)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn('SERVICE_ROLE_KEY is not set', result.stderr)


if __name__ == '__main__':
    unittest.main()
