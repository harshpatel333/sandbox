import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('install-ui.sh')

LOCAL_SECRETS = {
    'SERVICE_ROLE_KEY': 'service-role-secret',
    'INTERNAL_API_TOKEN': 'internal-secret',
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
        self.secrets = root / 'secrets'
        self.secrets.mkdir()
        (self.secrets / 'promotion-account-key.pem').write_text(PROMOTION_KEY)
        self.env = {
            'PATH': os.environ['PATH'],
            'DEVBOX_CONFIG_DIR': str(root / 'etc'),
            'DEVBOX_UI_SRC': str(root / 'src'),
            'DEVBOX_SECRETS_DIR': str(self.secrets),
        }

    def run_script(self, local):
        (self.secrets / 'local.env').write_text(''.join(f'{k}={v}\n' for k, v in local.items()))
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
        self.assertFalse((self.secrets / 'console.env').exists())

    def test_missing_shared_key_fails_with_its_name(self):
        local = {'ANON_KEY': 'anon-key', **LOCAL_SECRETS}
        del local['SERVICE_ROLE_KEY']

        result = self.run_script(local)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn('SERVICE_ROLE_KEY is not set', result.stderr)


if __name__ == '__main__':
    unittest.main()
