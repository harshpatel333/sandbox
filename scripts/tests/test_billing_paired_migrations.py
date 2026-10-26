"""Exercise launch guard replacements against canonical and actual ahead schemas.

DATABASE_URL must be a local disposable PostgreSQL database ending in _test.
Each case creates and drops its own database; no shared schema is reset.
"""
import os
from pathlib import Path
import subprocess
import unittest
from urllib.parse import urlsplit, urlunsplit
from uuid import uuid4

ROOT = Path(__file__).resolve().parents[2]
MIGRATIONS = sorted((ROOT / 'supabase/migrations').glob('*.sql'))
FIXTURE = Path(__file__).parent / 'fixtures/paired_billing'


class PairedMigrations(unittest.TestCase):
    def setUp(self):
        url = urlsplit(os.environ['DATABASE_URL'])
        if url.hostname not in ('localhost', '127.0.0.1') or not url.path.endswith('_test'):
            self.fail('DATABASE_URL must name a local disposable _test database')
        self.name = 'billing_pair_' + uuid4().hex + '_test'
        self.admin = urlunsplit(url._replace(path='/postgres'))
        self.url = urlunsplit(url._replace(path='/' + self.name))
        self.sql('CREATE DATABASE ' + self.name, admin=True)
        self.addCleanup(lambda: self.sql('DROP DATABASE ' + self.name + ' WITH (FORCE)', admin=True))
        for migration in MIGRATIONS:
            if migration.name.startswith('20261026'):
                break
            self.sql(migration.read_text())

    def sql(self, sql, admin=False, succeeds=True):
        result = subprocess.run(['psql', self.admin if admin else self.url, '-X', '-qAt', '-v', 'ON_ERROR_STOP=1'],
                                input=sql, text=True, capture_output=True)
        if succeeds:
            self.assertEqual(result.returncode, 0, result.stderr[-6000:])
        else:
            self.assertNotEqual(result.returncode, 0, 'unsafe schema unexpectedly accepted')
        return result.stdout.strip()

    def launch(self, after=None):
        for migration in MIGRATIONS:
            if migration.name.startswith('20261026'):
                self.sql('BEGIN;\n' + migration.read_text() + '\nCOMMIT;')
                if after:
                    after(migration.name)

    def test_canonical_full_stack(self):
        self.launch()
        self.assertEqual(self.sql("SELECT to_regclass('public.billing_contract_provider_binding') IS NULL"), 't')
        self.assertEqual(self.sql("SELECT count(*) FROM billing_plan_version WHERE family='pro' AND monthly_fee_cents=25000"), '1')

    def pair_schema(self):
        for migration in sorted(FIXTURE.glob('2026*.sql')):
            self.sql(migration.read_text())

    def test_partial_pair_fails_before_replacement(self):
        before = self.sql("SELECT md5(pg_get_functiondef('billing_contract_eligible(billing_commercial_contract,timestamptz,timestamptz)'::regprocedure))")
        for create, cleanup in [
            ('CREATE TABLE billing_provider_pair_revocation(contract_id uuid)', 'DROP TABLE billing_provider_pair_revocation'),
            ('ALTER TABLE billing_entitlement_work ADD COLUMN paired_invoice_cursor text', 'ALTER TABLE billing_entitlement_work DROP COLUMN paired_invoice_cursor'),
            ("CREATE FUNCTION billing_provider_pair_unknown() RETURNS boolean LANGUAGE sql AS 'SELECT true'", 'DROP FUNCTION billing_provider_pair_unknown()'),
        ]:
            with self.subTest(marker=create):
                self.sql(create)
                self.sql('BEGIN;\n' + next(p for p in MIGRATIONS if '45510_' in p.name).read_text() + '\nCOMMIT;', succeeds=False)
                self.assertEqual(self.sql("SELECT md5(pg_get_functiondef('billing_contract_eligible(billing_commercial_contract,timestamptz,timestamptz)'::regprocedure))"), before)
                self.sql(cleanup)

    def test_partial_pair_fails_at_payment_replacement(self):
        self.pair_schema()
        for migration in MIGRATIONS:
            if migration.name.startswith('20261026'):
                if '45538_' in migration.name:
                    break
                self.sql(migration.read_text())
        self.sql('ALTER TABLE billing_contract_provider_binding RENAME COLUMN fee_anchor TO unknown_fee_anchor')
        before = self.sql("SELECT md5(pg_get_functiondef('guard_billing_contract_payment()'::regprocedure))")
        self.sql('BEGIN;\n' + migration.read_text() + '\nCOMMIT;', succeeds=False)
        self.assertEqual(self.sql("SELECT md5(pg_get_functiondef('guard_billing_contract_payment()'::regprocedure))"), before)

    def test_ahead_schema_each_replacement(self):
        self.pair_schema()
        self.sql((FIXTURE / 'authority_cases.sql').read_text())
        def check(name):
            if '45510_' in name or '45538_' in name:
                self.assertEqual(self.sql((FIXTURE / 'assert_authority.sql').read_text()), 'paired authority preserved')
        self.launch(check)
        self.assertEqual(self.sql((FIXTURE / 'assert_authority.sql').read_text()), 'paired authority preserved')


if __name__ == '__main__':
    unittest.main()
