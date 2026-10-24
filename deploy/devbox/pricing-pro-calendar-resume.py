#!/usr/bin/env python3
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(file))
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


qa = module('provider_qa', 'pricing-pro-provider-qa.py')
diag = module('provider_diagnostic', 'pricing-pro-provider-diagnostic.py')
SOURCE_HASH = 'b98f905b20e5760676645c08446f6d992dec6c7125911a780485e6250a762174'
ACCOUNT = 'acct_1UST2ILIAkKd3qgh'
MESSAGE = 'The customer needs a valid email when `collection_method=send_invoice`.'


def validate_receipts(raw, diagnostic):
    qa.require(hashlib.sha256(raw).hexdigest() == SOURCE_HASH, 'Original source receipt hash differs')
    source = json.loads(raw)
    row, fields, key = diag.failed_request(source, ACCOUNT)
    qa.require(diagnostic.get('source_sha256') == SOURCE_HASH and diagnostic.get('account_id') == ACCOUNT,
               'Diagnostic source/account differs')
    requests = diagnostic.get('requests', [])
    qa.require(requests and requests[-1].get('method') == 'POST'
               and requests[-1].get('path') == '/v1/subscriptions' and requests[-1].get('status') == 400,
               'No verified failed original replay')
    error = requests[-1].get('error', {})
    qa.require(error.get('type') == 'invalid_request_error' and error.get('code') is None
               and error.get('param') is None and error.get('message') == MESSAGE,
               'Actual diagnostic cause differs; do not apply email repair')
    qa.require(diagnostic.get('customer_subscription_ids') == []
               and diagnostic.get('replay_idempotency_key') == key
               and diagnostic.get('fixtures') == {k: row[k] for k in ('clock', 'customer', 'product', 'price')},
               'Original replay scope differs')
    return source, row


def repair_customer(provider, row, run_id, save):
    prefix = 'dev-pro-qa:' + run_id + ':2027'
    customer_path = '/v1/customers/' + row['customer']
    customer = provider.request('GET', customer_path)
    product = provider.request('GET', '/v1/products/' + row['product'])
    qa.require(customer.get('id') == row['customer'] and customer.get('test_clock') == row['clock']
               and customer.get('name') == prefix and product.get('id') == row['product']
               and product.get('name') == prefix, 'Original fixture ownership differs')
    email = qa.fixture_email(run_id, 2027)
    qa.require(customer.get('email') in (None, '', email), 'Nonempty unrecognized email; no overwrite')
    if customer.get('email') != email:
        provider.request('POST', customer_path, {'email': email}, prefix + ':customer-email')
        save()
    verified = provider.request('GET', customer_path)
    qa.require(verified.get('id') == row['customer'] and verified.get('test_clock') == row['clock']
               and verified.get('email') == email and verified.get('name') == prefix,
               'Repaired customer verification differs')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state-dir', type=Path, default=Path('/var/lib/superserve-devbox'))
    parser.add_argument('--source-receipt', type=Path, required=True)
    parser.add_argument('--diagnostic-receipt', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--resume-calendar', action='store_true')
    args = parser.parse_args()
    descriptor = None
    result = {'status': 'blocked', 'requests': [], 'calendars': [], 'account_id': ACCOUNT}

    def save():
        encoded = (json.dumps(result, indent=2) + '\n').encode()
        os.lseek(descriptor, 0, os.SEEK_SET)
        os.ftruncate(descriptor, 0)
        os.write(descriptor, encoded)
        os.fsync(descriptor)

    try:
        qa.require(os.geteuid() == 0, 'Run root on the isolated integration box')
        raw = args.source_receipt.read_bytes()
        diagnostic_raw = args.diagnostic_receipt.read_bytes()
        source, row = validate_receipts(raw, json.loads(diagnostic_raw))
        descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        result.update(source_sha256=SOURCE_HASH, diagnostic_sha256=hashlib.sha256(diagnostic_raw).hexdigest(),
                      run_id=source['run_id'], original_fixtures={key: row[key] for key in ('clock', 'customer', 'product', 'price')},
                      original_request=source['requests'][-1], diagnostic_request=json.loads(diagnostic_raw)['requests'][-1])
        save()
        spec = importlib.util.spec_from_file_location('devbox_services', Path(__file__).parent / 'services/devbox_services.py')
        helpers = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helpers)
        config = helpers.read_env('/etc/superserve-devbox/devbox.env')
        qa.require(config.get('DEVBOX_NAME') == 'awille-ai-dev-billing' and config.get('DEVBOX_ID') == '29baf003ad05',
                   'Wrong isolated box')
        binding = json.loads((args.state_dir / 'pricing-stripe-binding.json').read_text())
        qa.require(binding.get('verified_test_mode') is True and binding.get('account_id') == ACCOUNT,
                   'Existing verified binding required')
        env = helpers.read_env(args.state_dir / 'api.env')
        reader = diag.Client(env.get('STRIPE_SECRET_KEY', ''), env.get('STRIPE_API_VERSION', ''), result, save)
        reader.bind(ACCOUNT)
        _, fields, _ = diag.failed_request(source, ACCOUNT)
        diag.inventory(reader, row, fields, result)
        result['status'] = 'inventory_complete_no_mutations'
        save()
        if args.resume_calendar:
            provider = qa.Provider(env.get('STRIPE_SECRET_KEY', ''), env.get('STRIPE_API_VERSION', ''), ACCOUNT,
                                   result['requests'])
            provider.bind()
            repair_customer(provider, row, source['run_id'], save)
            diag.inventory(reader, row, fields, result)
            qa.run_calendar(provider, 2027, source['run_id'], result, save, existing=row)
            qa.run_calendar(provider, 2028, source['run_id'], result, save)
            result['status'] = 'passed_resumed_calendar_control_only'
        qa.require(args.source_receipt.read_bytes() == raw and args.diagnostic_receipt.read_bytes() == diagnostic_raw,
                   'Original source or diagnostic receipt changed')
        save()
        print('Resume receipt: ' + str(args.output))
        return 0
    except (qa.QAError, diag.DiagnosticError, OSError, ValueError, TypeError, KeyError) as error:
        result['status'] = 'blocked'
        result['error'] = str(error) if isinstance(error, (qa.QAError, diag.DiagnosticError)) else 'Local configuration/receipt invalid'
        if descriptor is not None:
            save()
        print(result['error'])
        return 1
    finally:
        if descriptor is not None:
            os.close(descriptor)


if __name__ == '__main__':
    raise SystemExit(main())
