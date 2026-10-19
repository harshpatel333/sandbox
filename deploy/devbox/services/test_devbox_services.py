import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import devbox_services as ds  # noqa: E402

ROOT = Path(__file__).resolve().parents[3]


def fake_keypair():
    return "-----BEGIN PRIVATE KEY-----\nexample\n-----END PRIVATE KEY-----\n", base64.b64encode(b"k" * 32).decode()


def b64url_decode(part):
    return base64.urlsafe_b64decode(part + "=" * (-len(part) % 4))


class SecretsTest(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.env = self.dir / "secrets" / "local.env"
        self.key = self.dir / "secrets" / "key.pem"

    def test_first_run_generates_private_files(self):
        values = ds.ensure_secrets(self.env, self.key, keypair=fake_keypair)
        for key in [*ds.GENERATED, "ANON_KEY", "SERVICE_ROLE_KEY", "PROMOTION_ACCOUNT_PUBLIC_KEY"]:
            self.assertTrue(values[key], key)
        self.assertEqual(stat.S_IMODE(self.env.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(self.key.stat().st_mode), 0o600)
        self.assertEqual(ds.read_env(self.env), values)
        self.assertEqual(len(bytes.fromhex(values["SANDBOX_ACCESS_TOKEN_SEED"])), 32)
        self.assertEqual(len(base64.b64decode(values["SECRETS_SIGNING_KEY"], validate=True)), 32)
        self.assertNotEqual(values["PROMOTION_CAPTURE_TOKEN"], values["PROMOTION_ACCOUNT_TOKEN"])
        self.assertNotEqual(values["PROMOTION_ACCOUNT_TOKEN"], values["INTERNAL_API_TOKEN"])

    def test_rerun_never_rotates_existing_values(self):
        first = ds.ensure_secrets(self.env, self.key, keypair=fake_keypair)
        pem = self.key.read_text()

        def fail():
            raise AssertionError("existing key pair must be kept")

        self.assertEqual(ds.ensure_secrets(self.env, self.key, keypair=fail), first)
        self.assertEqual(self.key.read_text(), pem)

    def test_missing_values_are_added_without_touching_others(self):
        ds.write_env(self.env, {"POSTGRES_PASSWORD": "kept"})
        values = ds.ensure_secrets(self.env, self.key, keypair=fake_keypair)
        self.assertEqual(values["POSTGRES_PASSWORD"], "kept")
        self.assertIn("JWT_SECRET", values)

    def test_lost_private_key_is_regenerated_with_matching_public_key(self):
        ds.ensure_secrets(self.env, self.key, keypair=fake_keypair)
        self.key.unlink()
        replacement = ("-----BEGIN PRIVATE KEY-----\nnew\n-----END PRIVATE KEY-----\n", base64.b64encode(b"n" * 32).decode())
        values = ds.ensure_secrets(self.env, self.key, keypair=lambda: replacement)
        self.assertEqual(self.key.read_text(), replacement[0])
        self.assertEqual(values["PROMOTION_ACCOUNT_PUBLIC_KEY"], replacement[1])

    def test_supabase_keys_are_hs256_role_tokens(self):
        values = ds.generate_secrets({}, now=1_000)
        for key, role in (("ANON_KEY", "anon"), ("SERVICE_ROLE_KEY", "service_role")):
            header, payload, signature = values[key].split(".")
            expected = hmac.new(values["JWT_SECRET"].encode(), f"{header}.{payload}".encode(), hashlib.sha256).digest()
            self.assertEqual(b64url_decode(signature), expected)
            self.assertEqual(json.loads(b64url_decode(header)), {"alg": "HS256", "typ": "JWT"})
            claims = json.loads(b64url_decode(payload))
            self.assertEqual((claims["role"], claims["iat"]), (role, 1_000))
            self.assertGreater(claims["exp"], claims["iat"])

    def test_real_keypair_matches_api_encoding(self):
        pem, public = ds.ed25519_keypair()
        self.assertIn("BEGIN PRIVATE KEY", pem)
        self.assertEqual(len(base64.b64decode(public, validate=True)), 32)


class EnvFileTest(unittest.TestCase):
    def test_round_trips_values_that_need_shell_quoting(self):
        path = Path(tempfile.mkdtemp()) / "x.env"
        values = {"BILLING_RESOURCE_CONFIG": '[{"resource_key":"vcpu","stripe_price_id":"price_1"}]', "EMPTY": "",
                  "EMAIL": "Superserve <noreply@devbox.localhost>"}
        ds.write_env(path, values)
        self.assertEqual(ds.read_env(path), values)

    def test_rejects_multiline_values(self):
        with self.assertRaises(ValueError):
            ds.write_env(Path(tempfile.mkdtemp()) / "x.env", {"KEY": "a\nb"})


class DockerDaemonTest(unittest.TestCase):
    def test_merges_into_existing_config_and_reports_changes(self):
        path = Path(tempfile.mkdtemp()) / "daemon.json"
        path.write_text(json.dumps({"log-driver": "journald", "iptables": True}))
        self.assertTrue(ds.merge_docker_daemon(path))
        self.assertEqual(json.loads(path.read_text()),
                         {"log-driver": "journald", "iptables": False, "ip6tables": False, "bridge": "none"})
        self.assertFalse(ds.merge_docker_daemon(path))

    def test_creates_missing_config(self):
        path = Path(tempfile.mkdtemp()) / "daemon.json"
        self.assertTrue(ds.merge_docker_daemon(path))
        self.assertEqual(json.loads(path.read_text()), ds.DOCKER_DAEMON)


class ApiEnvTest(unittest.TestCase):
    def setUp(self):
        self.env = {
            **ds.generate_secrets({}, now=1),
            "PROMOTION_ACCOUNT_PUBLIC_KEY": "a2V5",
            "EMAIL_FROM": "Superserve <noreply@devbox.localhost>",
            "CONSOLE_URL": "http://localhost:3000",
            "EDGE_PROXY_DOMAIN": "sandbox.localhost",
            "DEVBOX_BUCKET": "superserve-devbox-0123456789ab",
            "SANDBOX_SHA": "abc",
            "STRIPE_SECRET_KEY": "sk_test_x",
            "STRIPE_WEBHOOK_SECRET": "whsec_x",
            "UNRELATED": "ignored",
        }
        self.runtime = {"host_id": "host-1", "host_region": "us-central1"}

    def test_box_settings(self):
        values = ds.api_env(self.env, self.runtime)
        self.assertEqual(values["API_HOST"], "127.0.0.1")
        self.assertEqual((values["EMAIL_TRANSPORT"], values["SMTP_HOST"], values["SMTP_PORT"]), ("smtp", "127.0.0.1", "1025"))
        self.assertEqual(values["BACKUP_BUCKET"], "superserve-devbox-0123456789ab")
        self.assertEqual((values["TEMPLATE_BUILD_REGION"], values["DEFAULT_HOST_ID"]), ("us-central1", "host-1"))
        self.assertEqual(values["VMD_GRPC_ADDRESS"], "127.0.0.1:50051")
        self.assertTrue(values["DATABASE_URL"].startswith(f"postgresql://postgres:{self.env['POSTGRES_PASSWORD']}@127.0.0.1:54322/"))
        self.assertTrue(values["PROMOTION_AUTH_DATABASE_URL"].startswith("postgresql://promotion_evidence_proxy:"))
        self.assertEqual((values["STRIPE_SECRET_KEY"], values["STRIPE_WEBHOOK_SECRET"]), ("sk_test_x", "whsec_x"))
        self.assertNotIn("UNRELATED", values)
        self.assertNotIn("STRIPE_PRO_MONTHLY_PRICE_ID", values)
        self.assertNotIn("JWT_SECRET", values)

    def test_missing_runtime_identity_fails(self):
        for key in ("host_id", "host_region"):
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, key):
                ds.api_env(self.env, {k: v for k, v in self.runtime.items() if k != key})


class ServicesManifestTest(unittest.TestCase):
    def test_lists_every_pinned_image_without_secrets(self):
        compose = Path(__file__).with_name("compose.yml")
        manifest = ds.services_manifest(compose, True, digest=lambda ref: "sha256:" + "0" * 64)
        self.assertEqual(set(manifest["images"]), set(ds.compose_images(compose)))
        for ref in manifest["images"]:
            self.assertRegex(ref, r":[\w.-]+$", "images must be pinned to a tag")
            self.assertNotIn(":latest", ref)
        self.assertTrue(manifest["stripe_listen"])


class MigrationToolTest(unittest.TestCase):
    def test_devbox_uses_the_deployment_cli_version(self):
        install = (Path(__file__).resolve().parents[1] / "install-api.sh").read_text()
        devbox = re.search(r"^SUPABASE_CLI_VERSION=(\S+)$", install, re.MULTILINE).group(1)
        deploy = re.search(r'^CLI_VERSION = "([^"]+)"$', (ROOT / "scripts/migrate_database.py").read_text(), re.MULTILINE).group(1)
        self.assertEqual(devbox, deploy)


if __name__ == "__main__":
    unittest.main()
