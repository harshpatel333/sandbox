#!/usr/bin/env python3
"""Manual diagnostic: one request per operation, no raw responses in output."""
import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

PROJECT = 'rayai-prod'
DEPLOYMENT = 'superserve-github-actions@rayai-prod.iam.gserviceaccount.com'
RUNTIME = 'superserve-controlplane-use4@rayai-prod.iam.gserviceaccount.com'
KEY = f'projects/{PROJECT}/locations/us-central1/keyRings/superserve/cryptoKeys/credentials-kek'
SECRETS = [
    'operator-api-token-use4', 'database-url', 'internal-api-token',
    'sandbox-access-token-seed', 'secretsproxy-signing-key', None,
    'sentry-dsn', 'system-team-id-production', 'slack-quota-alert-webhook',
    'posthog-project-key', 'stripe-secret-key-use', 'stripe-webhook-secret-use',
    'stripe-meter-error-webhook-secret-use',
]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def category(code, body):
    # Only fixed categories escape this boundary; provider messages may be sensitive.
    value = body.lower()
    if b'disabled' in value:
        return 'DISABLED_RESOURCE_OR_VERSION'
    if b'destroyed' in value:
        return 'DESTROYED_VERSION'
    return {400: 'INVALID_ARGUMENT', 401: 'UNAUTHENTICATED',
            403: 'PERMISSION_DENIED', 404: 'NOT_FOUND',
            409: 'CONFLICT', 412: 'FAILED_PRECONDITION',
            429: 'RESOURCE_EXHAUSTED'}.get(code, 'SERVICE_ERROR' if code >= 500 else 'HTTP_ERROR')


def report(operation, resource, status, error=None, http=None):
    result = dict(operation=operation, resource=resource, status=status)
    if error is not None:
        result['category'] = error
    if http is not None:
        result['http_status'] = http
    print(json.dumps(result, sort_keys=True), flush=True)


def request(opener, operation, resource, url, token, payload=None, discard=False):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = f'Bearer {token}'
    req = urllib.request.Request(url, data=data, headers=headers)
    try:
        with opener.open(req, timeout=30) as response:
            if discard:
                while response.read(65536):
                    pass
                result = {}
            else:
                result = json.loads(response.read(262144))
                if not isinstance(result, dict):
                    raise ValueError('invalid response type')
        report(operation, resource, 'PASS')
        return result
    except urllib.error.HTTPError as exc:
        with exc:
            body = exc.read(65536)
        report(operation, resource, 'FAIL', category(exc.code, body), exc.code)
    except (urllib.error.URLError, TimeoutError, OSError):
        report(operation, resource, 'FAIL', 'TRANSPORT_ERROR')
    except (ValueError, TypeError):
        report(operation, resource, 'FAIL', 'INVALID_RESPONSE')
    return None


def main(opener=None):
    opener = opener or urllib.request.build_opener(NoRedirect())
    oidc_url = os.environ.get('ACTIONS_ID_TOKEN_REQUEST_URL', '')
    oidc_request_token = os.environ.pop('ACTIONS_ID_TOKEN_REQUEST_TOKEN', '')
    provider = os.environ.get('WORKLOAD_IDENTITY_PROVIDER', '')
    parsed_url = urllib.parse.urlsplit(oidc_url)
    configured_secret = os.environ.get('COMPUTE_RESTRICTIONS_SECRET_NAME', '')
    if (os.environ.get('PROJECT') != PROJECT or
            os.environ.get('DEPLOYMENT_IDENTITY') != DEPLOYMENT or
            not oidc_request_token or parsed_url.scheme != 'https' or
            not (parsed_url.hostname or '').endswith('.actions.githubusercontent.com') or
            not re.fullmatch(r'projects/[0-9]+/locations/global/workloadIdentityPools/[A-Za-z0-9_-]+/providers/[A-Za-z0-9_-]+', provider) or
            not re.fullmatch(r'[A-Za-z0-9_-]+', configured_secret)):
        report('configuration', 'expected-production-bindings', 'FAIL', 'INVALID_CONFIGURATION')
        return 1
    report('identity', DEPLOYMENT, 'CONFIGURED')
    query = urllib.parse.parse_qs(parsed_url.query)
    query['audience'] = [f'https://iam.googleapis.com/{provider}']
    oidc_url = urllib.parse.urlunsplit(parsed_url._replace(query=urllib.parse.urlencode(query, doseq=True)))
    oidc = request(opener, 'auth.oidc', 'configured-github-provider', oidc_url, oidc_request_token)
    oidc_token = (oidc or {}).get('value')
    if not isinstance(oidc_token, str) or not oidc_token:
        report('auth.bootstrap', DEPLOYMENT, 'BLOCKED', 'NO_OIDC_TOKEN')
        return 1
    federated = request(opener, 'auth.federation', 'configured-github-provider',
        'https://sts.googleapis.com/v1/token', None, {
            'audience': f'//iam.googleapis.com/{provider}',
            'grantType': 'urn:ietf:params:oauth:grant-type:token-exchange',
            'requestedTokenType': 'urn:ietf:params:oauth:token-type:access_token',
            'scope': 'https://www.googleapis.com/auth/cloud-platform',
            'subjectTokenType': 'urn:ietf:params:oauth:token-type:jwt',
            'subjectToken': oidc_token})
    federated_token = (federated or {}).get('access_token')
    if not isinstance(federated_token, str) or not federated_token:
        report('auth.bootstrap', DEPLOYMENT, 'BLOCKED', 'NO_FEDERATED_TOKEN')
        return 1
    deployment = request(opener, 'auth.deployment-identity', DEPLOYMENT,
        f'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{DEPLOYMENT}:generateAccessToken',
        federated_token, {'scope': ['https://www.googleapis.com/auth/cloud-platform'], 'lifetime': '600s'})
    deployment_token = (deployment or {}).get('accessToken')
    if not isinstance(deployment_token, str) or not deployment_token:
        report('auth.bootstrap', DEPLOYMENT, 'BLOCKED', 'NO_DEPLOYMENT_TOKEN')
        return 1
    identity = request(opener, 'impersonation', RUNTIME,
        f'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{RUNTIME}:generateAccessToken',
        deployment_token, {'scope': ['https://www.googleapis.com/auth/cloud-platform'], 'lifetime': '600s'})
    token = (identity or {}).get('accessToken')
    if not isinstance(token, str) or not token:
        report('runtime-checks', RUNTIME, 'BLOCKED', 'NO_RUNTIME_TOKEN')
        return 1
    failures = 0
    plaintext = b'control-plane-kms-access-probe-v1\n'
    encrypted = request(opener, 'kms.encrypt', KEY,
        f'https://cloudkms.googleapis.com/v1/{KEY}:encrypt', token,
        {'plaintext': base64.b64encode(plaintext).decode()})
    ciphertext = (encrypted or {}).get('ciphertext')
    if isinstance(ciphertext, str) and ciphertext:
        decrypted = request(opener, 'kms.decrypt', KEY,
            f'https://cloudkms.googleapis.com/v1/{KEY}:decrypt', token, {'ciphertext': ciphertext})
        matches = (decrypted or {}).get('plaintext') == base64.b64encode(plaintext).decode()
        report('kms.round-trip', KEY, 'PASS' if matches else 'FAIL', None if matches else 'ROUND_TRIP_FAILED')
        failures += not matches
    else:
        report('kms.decrypt', KEY, 'BLOCKED', 'NO_CIPHERTEXT')
        failures += 1
    for name in SECRETS:
        secret = name or configured_secret
        label = name or 'configured-compute-restrictions-secret'
        result = request(opener, 'secret.access', label,
            f'https://secretmanager.googleapis.com/v1/projects/{PROJECT}/secrets/{secret}/versions/latest:access',
            token, discard=True)
        failures += result is None
    report('summary', RUNTIME, 'PASS' if failures == 0 else 'FAIL')
    return int(failures > 0)


if __name__ == '__main__':
    try:
        result = main()
    except Exception:
        report('diagnostic', 'runner', 'FAIL', 'UNEXPECTED_CLIENT_ERROR')
        result = 1
    sys.exit(result)
