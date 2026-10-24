#!/usr/bin/env python3
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request


class DiagnosticError(Exception):
    pass


def require(value, message):
    if not value:
        raise DiagnosticError(message)


def safe_message(value, key):
    if not isinstance(value, str):
        return None
    value = value.replace(key, '[credential]')
    value = re.sub(r'(?:[sr]k_(?:test|live)_|whsec_|pk_(?:test|live)_)[A-Za-z0-9_]+', '[credential]', value)
    value = re.sub(r'\b[A-Za-z0-9_]+_secret_[A-Za-z0-9_]+', '[client-secret]', value)
    value = re.sub(r'https?://\S+', '[url]', value)
    value = re.sub(r'[A-Za-z0-9_.+-]+@[A-Za-z0-9.-]+', '[email]', value)
    value = re.sub(r'\b\d{12,}\b', '[number]', value)
    return re.sub(r'\s+', ' ', value)[:400]


def error_fields(body, key):
    try:
        error = json.loads(body).get('error', {})
    except (ValueError, AttributeError):
        return {'type': 'unparseable'}
    if not isinstance(error, dict):
        return {'type': 'unparseable'}
    allowed_types = {'invalid_request_error', 'authentication_error', 'permission_error', 'rate_limit_error',
                     'api_error', 'card_error', 'idempotency_error'}
    result = {'type': error.get('type') if error.get('type') in allowed_types else 'unrecognized'}
    for name in ('code', 'param'):
        value = error.get(name)
        if value is None:
            result[name] = None
        elif isinstance(value, str) and re.fullmatch(r'[A-Za-z0-9_\[\].-]{1,160}', value):
            result[name] = '[redacted]' if safe_message(value, key) != value else value
        else:
            result[name] = '[redacted]'
    result['message'] = safe_message(error.get('message'), key)
    return result


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Client:
    def __init__(self, key, version, result, save):
        require(re.fullmatch(r'[sr]k_test_[A-Za-z0-9]+', key), 'Test-mode credential required')
        require(version == '2026-05-27.dahlia', 'Failed request API version differs; review instead of replay')
        self.key, self.version, self.result, self.save = key, version, result, save
        self.opener = urllib.request.build_opener(NoRedirect(), urllib.request.ProxyHandler({}))
        self.bound = False

    def request(self, method, path, fields=None, idempotency=None):
        require(method in ('GET', 'POST') and path.startswith('/v1/') and '?' not in path and '..' not in path,
                'Invalid diagnostic request')
        require(self.bound or (method == 'GET' and path in ('/v1/account', '/v1/balance')), 'Binding required')
        require(method == 'GET' or (path == '/v1/subscriptions' and idempotency), 'Only exact subscription replay permitted')
        headers = {'Authorization': 'Bearer ' + self.key, 'Stripe-Version': self.version}
        encoded = urllib.parse.urlencode(fields or {})
        url = 'https://api.stripe.com' + path
        data = None
        if method == 'GET' and encoded:
            url += '?' + encoded
        if method == 'POST':
            data = encoded.encode()
            headers.update({'Content-Type': 'application/x-www-form-urlencoded', 'Idempotency-Key': idempotency})
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        observation = {'method': method, 'path': path}
        try:
            with self.opener.open(req, timeout=30) as response:
                body = json.load(response)
                observation.update(status=response.status, request_id=response.headers.get('Request-Id'))
        except urllib.error.HTTPError as error:
            observation.update(status=error.code, request_id=error.headers.get('Request-Id'),
                               error=error_fields(error.read(65536), self.key))
            self.result['requests'].append(observation)
            self.save()
            raise DiagnosticError(f'Stripe {method} {path} HTTP {error.code}; sanitized details in receipt') from None
        except (OSError, ValueError):
            raise DiagnosticError(f'Stripe {method} {path} response unavailable; inventory before any retry') from None
        require(body.get('livemode') is not True, 'Live provider object rejected')
        self.result['requests'].append(observation)
        self.save()
        return body

    def bind(self, account):
        require(self.request('GET', '/v1/account').get('id') == account, 'Stripe account differs')
        require(self.request('GET', '/v1/balance').get('livemode') is False, 'Test balance required')
        self.bound = True


def failed_request(source, expected_account):
    require(source.get('status') == 'blocked' and source.get('verified_test_mode') is True,
            'Source must be the preserved failed verified-test run')
    require(source.get('account_id') == expected_account
            and source.get('api_version') == '2026-05-27.dahlia', 'Source account/version differs')
    run = source.get('run_id')
    require(isinstance(run, str) and re.fullmatch(r'[0-9a-f]{32}', run), 'Missing original run ID')
    rows = source.get('calendars', [])
    require(len(rows) == 1 and rows[0].get('year') == 2027 and rows[0].get('status') == 'pending'
            and 'subscription' not in rows[0], 'Source is not the first failed subscription stage')
    row = rows[0]
    for name, prefix in [('clock', 'clock_'), ('customer', 'cus_'), ('product', 'prod_'), ('price', 'price_')]:
        require(isinstance(row.get(name), str) and re.fullmatch(prefix + r'[A-Za-z0-9_]+', row[name]),
                'Missing original ' + name + ' identity')
    requests = source.get('requests', [])
    require(requests and requests[-1].get('method') == 'POST' and requests[-1].get('path') == '/v1/subscriptions'
            and requests[-1].get('status') == 400, 'Original failure differs')
    prefix = 'dev-pro-qa:' + run + ':2027'
    fields = {'customer': row['customer'], 'items[0][price]': row['price'],
              'collection_method': 'send_invoice', 'days_until_due': 30, 'proration_behavior': 'none',
              'billing_mode[type]': 'flexible', 'metadata[dev_pro_qa]': run}
    return row, fields, prefix + ':subscription'


def inventory(client, row, fields, result):
    customer = client.request('GET', '/v1/customers/' + row['customer'])
    require(customer.get('id') == row['customer'] and customer.get('test_clock') == row['clock'],
            'Customer/clock identity differs')
    product = client.request('GET', '/v1/products/' + row['product'])
    price = client.request('GET', '/v1/prices/' + row['price'])
    require(product.get('id') == row['product'] and price.get('id') == row['price']
            and price.get('product') == row['product'] and price.get('unit_amount') == 25000
            and price.get('currency') == 'usd' and price.get('recurring', {}).get('interval') == 'month',
            'Original fixture price/product differs')
    clock = client.request('GET', '/v1/test_helpers/test_clocks/' + row['clock'])
    require(clock.get('id') == row['clock'] and clock.get('status') == 'ready'
            and clock.get('frozen_time') == 1801396800, 'Original clock changed; do not replay')
    created = clock.get('created')
    require(isinstance(created, int) and 0 <= time.time() - created < 23 * 3600,
            'Fixture is outside safe replay retention; reconcile without mutation')
    subs = client.request('GET', '/v1/subscriptions', {'customer': fields['customer'], 'status': 'all', 'limit': 100})
    require(subs.get('has_more') is False and isinstance(subs.get('data'), list), 'Incomplete customer subscription inventory')
    result['customer_subscription_ids'] = [s.get('id') for s in subs['data']]
    require(not subs['data'], 'Customer already has a subscription; reconcile instead of replay')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state-dir', type=Path, default=Path('/var/lib/superserve-devbox'))
    parser.add_argument('--source-receipt', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--replay-failed-subscription', action='store_true')
    args = parser.parse_args()
    result = {'status': 'blocked', 'requests': [], 'account_id': 'acct_1UST2ILIAkKd3qgh'}
    descriptor = None

    def save():
        encoded = (json.dumps(result, indent=2) + '\n').encode()
        os.lseek(descriptor, 0, os.SEEK_SET)
        os.ftruncate(descriptor, 0)
        os.write(descriptor, encoded)
        os.fsync(descriptor)

    try:
        require(os.geteuid() == 0, 'Run root on the isolated box')
        raw = args.source_receipt.read_bytes()
        source = json.loads(raw)
        row, fields, idempotency = failed_request(source, result['account_id'])
        result.update(source_sha256=hashlib.sha256(raw).hexdigest(),
                      original_request={key: source['requests'][-1].get(key) for key in ('method', 'path', 'status', 'request_id')},
                      fixtures={key: row[key] for key in ('clock', 'customer', 'product', 'price')},
                      replay_idempotency_key=idempotency)
        descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        save()
        spec = importlib.util.spec_from_file_location('devbox_services', Path(__file__).parent / 'services/devbox_services.py')
        helpers = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helpers)
        config = helpers.read_env('/etc/superserve-devbox/devbox.env')
        require(config.get('DEVBOX_NAME') == 'awille-ai-dev-billing' and config.get('DEVBOX_ID') == '29baf003ad05',
                'Wrong integration box')
        binding = json.loads((args.state_dir / 'pricing-stripe-binding.json').read_text())
        require(binding.get('verified_test_mode') is True and binding.get('account_id') == result['account_id'],
                'Existing verified account binding required')
        env = helpers.read_env(args.state_dir / 'api.env')
        client = Client(env.get('STRIPE_SECRET_KEY', ''), env.get('STRIPE_API_VERSION', ''), result, save)
        runner = Path(__file__).with_name('pricing-pro-provider-qa.py')
        require(hashlib.sha256(runner.read_bytes()).hexdigest() ==
                '519bd7856702f9869a937ea1d93efc92011892e48f9f3cb037547d03820cde1d',
                'Original runner source differs; review exact failed payload before replay')
        client.bind(result['account_id'])
        inventory(client, row, fields, result)
        result['status'] = 'inventory_complete_no_subscription'
        save()
        if args.replay_failed_subscription:
            result['replay_fields_sha256'] = hashlib.sha256(urllib.parse.urlencode(fields).encode()).hexdigest()
            save()
            subscription = client.request('POST', '/v1/subscriptions', fields, idempotency)
            result.update(status='exact_replay_succeeded_stop_for_reconciliation', subscription_id=subscription.get('id'))
        require(args.source_receipt.read_bytes() == raw, 'Original receipt changed during diagnostic')
        save()
        print('Diagnostic receipt: ' + str(args.output))
        return 0
    except (DiagnosticError, OSError, ValueError, TypeError, KeyError) as error:
        result['error'] = str(error) if isinstance(error, DiagnosticError) else 'Local receipt/configuration invalid; inspect on-box'
        if descriptor is not None:
            save()
        print(result['error'])
        return 1
    finally:
        if descriptor is not None:
            os.close(descriptor)


if __name__ == '__main__':
    raise SystemExit(main())
