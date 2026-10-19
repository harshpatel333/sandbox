import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('install-ui.sh')

SHARED_SECRETS = {
    'SUPABASE_SERVICE_ROLE_KEY': 'service-role-secret',
    'INTERNAL_API_TOKEN': 'internal-secret',
    'SANDBOX_INTERNAL_API_TOKEN': 'sandbox-internal-secret',
    'PROMOTION_CAPTURE_TOKEN': 'capture-secret',
    'PROMOTION_ACCOUNT_TOKEN': 'account-secret',
    'PROMOTION_ACCOUNT_PRIVATE_KEY': 'private-key-secret',
    'YC_CLAIM_CONSOLE_TOKEN': 'yc-secret',
}
CI_SECRETS = {'RECAPTCHA_API_KEY': 'recaptcha-secret', 'RECAPTCHA_PROJECT_ID': 'recaptcha-project'}


class InstallUiDryRunTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        (root / 'etc').mkdir()
        (root / 'etc' / 'devbox.env').write_text('RECAPTCHA_SITE_KEY=site-key\n')
        (root / 'etc' / 'secrets.env').write_text(''.join(f'{k}={v}\n' for k, v in CI_SECRETS.items()))
        (root / 'src' / 'apps' / 'console').mkdir(parents=True)
        (root / 'src' / 'apps' / 'console' / 'package.json').write_text('{}')
        self.shared = root / 'shared.env'
        self.env = {
            'PATH': os.environ['PATH'],
            'DEVBOX_CONFIG_DIR': str(root / 'etc'),
            'DEVBOX_UI_SRC': str(root / 'src'),
            'DEVBOX_SECRETS_DIR': str(root / 'secrets'),
            'DEVBOX_SHARED_SECRETS_FILE': str(self.shared),
        }

    def run_script(self, shared):
        self.shared.write_text(''.join(f'{k}={v}\n' for k, v in shared.items()))
        return subprocess.run([str(SCRIPT), '--dry-run'], env=self.env, capture_output=True, text=True)

    def test_build_args_carry_public_values_and_no_server_secret(self):
        result = self.run_script({'NEXT_PUBLIC_SUPABASE_ANON_KEY': 'anon-key', **SHARED_SECRETS})

        self.assertEqual(result.returncode, 0, result.stderr)
        build_args = [line for line in result.stdout.splitlines() if line.startswith('build-arg ')]
        self.assertIn('build-arg NEXT_PUBLIC_SUPABASE_ANON_KEY=anon-key', build_args)
        self.assertIn('build-arg NEXT_PUBLIC_RECAPTCHA_SITE_KEY=site-key', build_args)
        self.assertIn('build-arg NEXT_PUBLIC_SANDBOX_HOST=sandbox.localhost', build_args)
        for secret in [*SHARED_SECRETS.values(), *CI_SECRETS.values()]:
            self.assertNotIn(secret, result.stdout)
        self.assertFalse((Path(self.env['DEVBOX_SECRETS_DIR'])).exists())

    def test_missing_shared_secret_fails_with_its_name(self):
        shared = {'NEXT_PUBLIC_SUPABASE_ANON_KEY': 'anon-key', **SHARED_SECRETS}
        del shared['PROMOTION_ACCOUNT_PRIVATE_KEY']

        result = self.run_script(shared)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn('PROMOTION_ACCOUNT_PRIVATE_KEY is not set', result.stderr)


if __name__ == '__main__':
    unittest.main()
