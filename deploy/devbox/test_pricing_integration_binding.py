import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).parent


class Response(io.StringIO):
    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


class BindingTest(unittest.TestCase):
    def preflight(self, account, root):
        original_path = Path
        receipt = root / 'binding.json'
        def path(value):
            return receipt if str(value) == '/var/lib/superserve-devbox/pricing-stripe-binding.json' else original_path(value)
        responses = [Response(json.dumps({'id': account})), Response(json.dumps({'livemode': False}))]
        with patch.dict(os.environ, {'STRIPE_SECRET_KEY': 'rk_test_fixture'}, clear=True), patch('pathlib.Path', side_effect=path), patch('urllib.request.urlopen', side_effect=responses) as network, patch('sys.stdout', new_callable=io.StringIO):
            runpy.run_path(str(ROOT / 'pricing-integration-provider.py'))
        return receipt, network.call_count

    def test_wrong_valid_test_account_cannot_clobber_existing_binding(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            receipt = root / 'binding.json'
            receipt.write_bytes(b'prior receipt')
            with self.assertRaisesRegex(SystemExit, 'independently expected DEV account'):
                self.preflight('acct_other_fixture', root)
            self.assertEqual(receipt.read_bytes(), b'prior receipt')

    def test_expected_test_account_and_balance_produce_verified_binding(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt, calls = self.preflight('acct_1UST2ILIAkKd3qgh', Path(directory))
            self.assertEqual(calls, 2)
            self.assertTrue(json.loads(receipt.read_text())['verified_test_mode'])

    def test_dry_run_skips_provider_receipt_and_network_entry(self):
        source = (ROOT / 'services-up.sh').read_text().split('# Docker must never manage iptables here:', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'services').mkdir()
            (root / 'services/common.sh').write_text('load_box_config() { DEVBOX_NAME=awille-ai-dev-billing; DEVBOX_ID=29baf003ad05; }\ndie() { exit 1; }\n')
            script = root / 'services-up.sh'
            script.write_text(source)
            (root / 'pricing-integration-provider.py').write_text('from pathlib import Path\nPath(__file__).with_name("provider-called").touch()\n')
            subprocess.run(['bash', str(script), '--dry-run'], check=True, capture_output=True)
            self.assertFalse((root / 'provider-called').exists())
            subprocess.run(['bash', str(script)], check=True, capture_output=True)
            self.assertTrue((root / 'provider-called').exists())


if __name__ == '__main__':
    unittest.main()
