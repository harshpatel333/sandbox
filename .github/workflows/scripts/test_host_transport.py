"""A host deploying onto itself must run exactly the fleet's on-host scripts."""
import importlib.util
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import host_transport

SCRIPTS = Path(__file__).parent


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


deploy_vmd = load('transport_deploy_vmd', 'deploy-vmd.py')
deploy_proxy = load('transport_deploy_proxy', 'deploy-proxy-legacy.py')

LOCAL = dict(DEPLOY_TRANSPORT='local', DEPLOY_INSTANCE='example-host', DEPLOY_ZONE='us-central1-a')


def exercise(module, env):
    commands = []

    def run(args, **kwargs):
        commands.append(args)
        listing = args[:4] == ['gcloud', 'compute', 'instances', 'list']
        return subprocess.CompletedProcess(args, 0, 'example-host,us-central1-a\n' if listing else '', '')

    with patch.dict(os.environ, env, clear=True), patch.object(module.subprocess, 'run', side_effect=run), \
            patch.object(module.os.path, 'getsize', return_value=100), \
            patch.object(module.os.path, 'exists', return_value=True), patch('builtins.print'):
        result = module.main()
    return result, commands


def scripts(commands):
    out = []
    for args in commands:
        if args[:2] == ['bash', '-c']:
            out.append(args[2])
        elif args[:3] == ['gcloud', 'compute', 'ssh']:
            out.append(args[args.index('--command') + 1])
    return out


class LocalTransportTests(unittest.TestCase):
    def assert_local_matches_fleet(self, module, env):
        fleet_result, fleet = exercise(module, dict(env, GCP_PROJECT='example-project'))
        local_result, local = exercise(module, dict(env, **LOCAL))
        self.assertEqual((fleet_result, local_result), (0, 0))
        self.assertFalse([c for c in local if c[0] in ('gcloud', 'ssh-keygen')], local)
        self.assertTrue(scripts(local))
        self.assertEqual(scripts(local), scripts(fleet))
        copies = [c for c in local if c[0] == 'cp']
        uploads = [c for c in fleet if c[:3] == ['gcloud', 'compute', 'scp']]
        self.assertEqual([c[1:] for c in copies], [[u[3], u[4].split(':', 1)[1]] for u in uploads])

    def test_vmd_local_deploy_runs_the_fleet_scripts_without_gcloud(self):
        self.assert_local_matches_fleet(deploy_vmd, dict(
            SHA='1234567890abcdef', CONTROL_PLANE_URL='http://127.0.0.1:8080',
            DATABASE_URL='postgres://example.test/db', INTERNAL_API_TOKEN='example-token'))

    def test_proxy_local_deploy_runs_the_fleet_scripts_without_gcloud(self):
        self.assert_local_matches_fleet(deploy_proxy, dict(
            SHA='12345678', GCP_REGION='us-central1', PROXY_DOMAIN='sandbox.localhost',
            PROXY_PEERLESS='1', PROXY_PUBLIC_SCHEME='http', PROXY_HEALTH_URL='http://127.0.0.1:8090/health'))

    def test_incomplete_or_unknown_transport_is_refused(self):
        for env in (dict(DEPLOY_TRANSPORT='local'), dict(LOCAL, DEPLOY_ZONE=''), dict(DEPLOY_TRANSPORT='ssh')):
            with self.subTest(env=env), self.assertRaises(ValueError):
                host_transport.local_instance(env)

    def test_peerless_proxy_excludes_peer_settings(self):
        base = dict(LOCAL, SHA='12345678', GCP_REGION='us-central1', PROXY_DOMAIN='sandbox.localhost',
                    PROXY_PEERLESS='1')
        for extra in (dict(PEER_ROUTING_ENABLED='1', PROXY_DATABASE_URL='postgres://example.test/db'),
                      dict(PEER_PROXY_LISTEN_ADDR='auto'), dict(PEER_IDENTITY_HOSTS='example-host')):
            with self.subTest(extra=extra):
                result, commands = exercise(deploy_proxy, dict(base, **extra))
                self.assertEqual(result, 1)
                self.assertEqual(commands, [])


if __name__ == '__main__':
    unittest.main()
