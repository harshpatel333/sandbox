"""Exercise deployment's Supabase CLI against disposable Docker PostgreSQL."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
import uuid


ROOT = Path(__file__).resolve().parents[1]
CLI = os.environ.get("SUPABASE_CLI", "supabase")


def run(*args, **kwargs):
    return subprocess.run(args, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, timeout=120, **kwargs)


class MigrationCLITest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.container = "migration-cli-" + uuid.uuid4().hex[:12]
        cls.addClassCleanup(run, "docker", "rm", "-f", cls.container)
        result = run("docker", "run", "-d", "--name", cls.container,
                     "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
                     "-p", "127.0.0.1::5432", "postgres:17.6")
        if result.returncode:
            raise RuntimeError(result.stdout)
        for _ in range(60):
            # The image's temporary initialization server only accepts sockets.
            if run("docker", "exec", cls.container,
                   "pg_isready", "-h", "127.0.0.1", "-U", "postgres").returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError("disposable PostgreSQL did not become ready")
        port = run("docker", "port", cls.container, "5432").stdout.strip()
        cls.port = port.rsplit(":", 1)[1]

    def setUp(self):
        self.database = "test_" + uuid.uuid4().hex[:12]
        result = run("docker", "exec", self.container, "createdb",
                     "-U", "postgres", self.database)
        self.assertEqual(result.returncode, 0, result.stdout)
        self.temp = tempfile.TemporaryDirectory(prefix="migration-cli-")
        self.addCleanup(self.temp.cleanup)
        self.project = Path(self.temp.name)
        self.migrations = self.project / "supabase" / "migrations"
        self.migrations.mkdir(parents=True)
        (self.project / "supabase" / "config.toml").write_text(
            'project_id = "migration-cli-test"\n')

    def copy_migrations(self, through=None):
        for source in sorted((ROOT / "supabase" / "migrations").glob("*.sql")):
            if through is None or source.name.split("_")[0] <= through:
                shutil.copyfile(source, self.migrations / source.name)

    def sql(self, sql):
        result = run("docker", "exec", "-i", self.container, "psql",
                     "-XAt", "-v", "ON_ERROR_STOP=1", "-U", "postgres",
                     "-d", self.database, input=sql)
        self.assertEqual(result.returncode, 0, result.stdout)
        return result.stdout.strip()

    def push(self, error=None):
        result = run(CLI, "db", "push", "--db-url",
                     f"postgresql://postgres@127.0.0.1:{self.port}/{self.database}?sslmode=disable",
                     "--workdir", str(self.project), "--yes")
        if error is None:
            self.assertEqual(result.returncode, 0, result.stdout)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn(error, result.stdout)
        return result

    def history(self):
        return self.sql("SELECT row_to_json(m) FROM supabase_migrations.schema_migrations m "
                        "ORDER BY version")

    def assert_no_pending_migrations(self):
        count = len(list((ROOT / "supabase" / "migrations").glob("*.sql")))
        self.assertEqual(self.sql("SELECT count(*) FROM supabase_migrations.schema_migrations"),
                         str(count))
        history = self.history()
        self.push()
        self.assertEqual(self.history(), history)

    def test_fresh_database(self):
        self.copy_migrations()
        self.push()
        self.assert_no_pending_migrations()


class BillingRolesMigrationCLITest(MigrationCLITest):
    test_fresh_database = None

    def test_fresh_database_with_billing_roles(self):
        self.sql("CREATE ROLE anon; CREATE ROLE authenticated; CREATE ROLE service_role")
        self.copy_migrations()
        self.push()
        tables = ("billing_pro_upgrade_setup", "billing_pro_upgrade",
                  "billing_pro_upgrade_catchup", "billing_pro_released_invoice")
        for table in tables:
            with self.subTest(table=table):
                self.assertEqual(self.sql(
                    f"SELECT relrowsecurity FROM pg_class WHERE oid='public.{table}'::regclass"), "t")
                for role in ("anon", "authenticated", "service_role"):
                    for privilege in ("SELECT", "INSERT", "UPDATE", "DELETE"):
                        expected = "t" if role == "service_role" and privilege != "DELETE" else "f"
                        self.assertEqual(self.sql(
                            f"SELECT has_table_privilege('{role}','public.{table}','{privilege}')"), expected)
                self.assertEqual(self.sql(
                    "SELECT count(*) FROM pg_policies WHERE schemaname='public' "
                    f"AND tablename='{table}' AND roles=ARRAY['service_role']::name[] "
                    "AND cmd='ALL' AND qual='true' AND with_check='true'"), "1")
        self.assert_no_pending_migrations()


class InvoiceCashAuthorityMigrationCLITest(MigrationCLITest):
    test_fresh_database = None

    def test_cash_authority_without_supabase_roles(self):
        self.copy_migrations()
        self.push()
        self.assertEqual(self.sql(
            "SELECT count(*) FROM pg_roles WHERE rolname IN ('anon','authenticated','service_role')"), "0")
        self.assertEqual(self.sql(
            "SELECT relrowsecurity FROM pg_class WHERE oid='public.billing_invoice_cash_authority'::regclass"), "t")
        for function in ("guard_invoice_cash_authority()", "resolve_invoice_cash_authority(text)",
                         "guard_invoice_cash_intent()", "guard_frozen_invoice_plan_history()"):
            self.assertEqual(self.sql(
                "SELECT count(*) FROM pg_proc p CROSS JOIN LATERAL "
                "aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a "
                f"WHERE p.oid='public.{function}'::regprocedure "
                "AND a.grantee=0 AND a.privilege_type='EXECUTE'"), "0")
        self.assert_no_pending_migrations()


class InvoiceCashAuthorityDefaultACLMigrationCLITest(MigrationCLITest):
    test_fresh_database = None

    def test_cash_authority_resets_backend_default_all(self):
        self.sql("CREATE ROLE anon; CREATE ROLE authenticated; CREATE ROLE service_role")
        self.sql("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO service_role; "
                 "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON FUNCTIONS TO service_role")
        self.copy_migrations()
        self.push()
        for role in ("anon", "authenticated", "service_role"):
            for privilege in ("SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"):
                with self.subTest(role=role, privilege=privilege):
                    expected = "t" if role == "service_role" and privilege in ("SELECT", "INSERT") else "f"
                    self.assertEqual(self.sql(
                        f"SELECT has_table_privilege('{role}','public.billing_invoice_cash_authority','{privilege}')"), expected)
            for function in ("guard_invoice_cash_authority()", "resolve_invoice_cash_authority(text)",
                             "guard_invoice_cash_intent()", "guard_frozen_invoice_plan_history()"):
                expected = "t" if role == "service_role" and function.startswith("resolve_") else "f"
                self.assertEqual(self.sql(
                    f"SELECT has_function_privilege('{role}','public.{function}','EXECUTE')"), expected)
        self.assertEqual(self.sql(
            "SELECT count(*) FROM pg_policies WHERE schemaname='public' "
            "AND tablename='billing_invoice_cash_authority' AND roles=ARRAY['service_role']::name[] "
            "AND cmd='ALL' AND qual='true' AND with_check='true'"), "1")
        self.assert_no_pending_migrations()


if __name__ == "__main__":
    unittest.main(verbosity=2)
