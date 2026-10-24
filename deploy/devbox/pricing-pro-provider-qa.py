#!/usr/bin/env python3
import argparse
import calendar
from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class QAError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def require(value, message):
    if not value:
        raise QAError(message)


class Provider:
    def __init__(self, key, version, account, evidence):
        require(re.fullmatch(r'[sr]k_test_[A-Za-z0-9]+', key), 'Test-mode credential required')
        require(re.fullmatch(r'acct_[A-Za-z0-9]+', account), 'Expected test account required')
        require(bool(version) and '\n' not in version and '\r' not in version, 'API version required')
        self.key, self.version, self.account = key, version, account
        self.evidence = evidence
        self.opener = urllib.request.build_opener(NoRedirect(), urllib.request.ProxyHandler({}))
        self.bound = False

    def request(self, method, path, params=None, operation=None):
        require(path.startswith('/v1/') and '?' not in path and '..' not in path, 'Invalid provider path')
        require(self.bound or (method == 'GET' and path in ('/v1/account', '/v1/balance')), 'Account binding must precede provider work')
        require(method in ('GET', 'POST'), 'Unsupported provider method')
        headers = {'Authorization': 'Bearer ' + self.key, 'Stripe-Version': self.version}
        data = None
        if method == 'POST':
            require(bool(operation), 'Mutation requires stable idempotency key')
            headers['Idempotency-Key'] = operation
            headers['Content-Type'] = 'application/x-www-form-urlencoded'
            data = urllib.parse.urlencode(params or {}).encode()
        req = urllib.request.Request('https://api.stripe.com' + path, headers=headers, data=data, method=method)
        try:
            with self.opener.open(req, timeout=30) as response:
                result = json.load(response)
                self.evidence.append({'method': method, 'path': path, 'status': response.status,
                                      'request_id': response.headers.get('Request-Id')})
        except urllib.error.HTTPError as error:
            self.evidence.append({'method': method, 'path': path, 'status': error.code,
                                  'request_id': error.headers.get('Request-Id')})
            raise QAError(f'Stripe {method} {path}: HTTP {error.code}; inspect provider request log') from None
        except (OSError, ValueError):
            raise QAError(f'Stripe {method} {path}: response unavailable; reconcile before retry') from None
        require(result.get('livemode') is not True, 'Provider returned a live object')
        return result

    def bind(self):
        account = self.request('GET', '/v1/account')
        require(account.get('id') == self.account, 'Expected Stripe account differs')
        balance = self.request('GET', '/v1/balance')
        require(balance.get('livemode') is False, 'Balance did not confirm test mode')
        self.bound = True


def timestamp(year, month, day):
    return int(datetime(year, month, day, 12, tzinfo=timezone.utc).timestamp())


def period(subscription):
    items = subscription.get('items', {}).get('data', [])
    require(len(items) == 1, 'Calendar fixture has unexpected items')
    return items[0].get('current_period_start'), items[0].get('current_period_end')


def ready(provider, clock, target):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        obj = provider.request('GET', '/v1/test_helpers/test_clocks/' + clock)
        if obj.get('status') == 'ready':
            require(obj.get('frozen_time') == target, 'Clock time differs')
            return
        require(obj.get('status') == 'advancing', 'Test clock is not advancing or ready')
        time.sleep(2)
    raise QAError('Clock advance timed out; retain IDs and reconcile before retry')


def fixture_email(run_id, year):
    require(re.fullmatch(r'[0-9a-f]{32}', run_id) and year in (2027, 2028), 'Invalid fixture email scope')
    return f'dev-pro-qa-{run_id}-{year}@example.com'


def run_calendar(provider, year, run_id, result, save, existing=None):
    prefix = 'dev-pro-qa:' + run_id + ':' + str(year)
    start = timestamp(year, 1, 31)
    feb = timestamp(year, 2, calendar.monthrange(year, 2)[1])
    march = timestamp(year, 3, 31)
    row = dict(existing) if existing is not None else {'year': year, 'status': 'pending', 'expected_boundaries': [start, feb, march]}
    result['calendars'].append(row)

    def create(label, path, fields):
        obj = provider.request('POST', path, fields, prefix + ':' + label)
        row[label] = obj.get('id')
        require(isinstance(row[label], str) and re.fullmatch(r'[A-Za-z0-9_]+', row[label]), 'Missing provider object ID')
        save()
        return obj

    if existing is None:
        clock = create('clock', '/v1/test_helpers/test_clocks', {'frozen_time': start, 'name': prefix})
        require(clock.get('livemode') is False, 'Clock did not confirm test mode')
        customer = create('customer', '/v1/customers', {'test_clock': clock['id'], 'name': prefix,
                          'email': fixture_email(run_id, year)})
        product = create('product', '/v1/products', {'name': prefix})
        price = create('price', '/v1/prices', {'product': product['id'], 'currency': 'usd', 'unit_amount': 25000,
                                            'recurring[interval]': 'month'})
    else:
        customer, price = {'id': row['customer']}, {'id': row['price']}
        clock = {'id': row['clock']}
    sub = create('subscription', '/v1/subscriptions', {'customer': customer['id'], 'items[0][price]': price['id'],
                   'collection_method': 'send_invoice', 'days_until_due': 30, 'proration_behavior': 'none',
                   'billing_mode[type]': 'flexible', 'metadata[dev_pro_qa]': run_id})
    require(period(sub) == (start, feb), 'Jan31 initial monthly period differs')
    anchor = sub.get('billing_cycle_anchor')
    path = '/v1/subscriptions/' + sub['id']
    provider.request('POST', path, {'pause_collection[behavior]': 'keep_as_draft'}, prefix + ':hold')
    held = provider.request('GET', path)
    require(held.get('pause_collection', {}).get('behavior') == 'keep_as_draft', 'Subscription hold unavailable')
    require(period(held) == (start, feb) and held.get('billing_cycle_anchor') == anchor, 'Hold changed calendar')
    provider.request('POST', path, {'pause_collection': '', 'billing_cycle_anchor': 'unchanged',
                                  'proration_behavior': 'none'}, prefix + ':release-hold')
    resumed = provider.request('GET', path)
    require(not resumed.get('pause_collection') and period(resumed) == (start, feb)
            and resumed.get('billing_cycle_anchor') == anchor, 'Hold release changed calendar')
    row['control_verified'] = True
    for label, target, expected_end in [('february', feb, march), ('march', march, timestamp(year, 4, 30))]:
        provider.request('POST', '/v1/test_helpers/test_clocks/' + clock['id'] + '/advance',
                         {'frozen_time': target}, prefix + ':advance:' + label)
        ready(provider, clock['id'], target)
        current = provider.request('GET', path)
        require(period(current) == (target, expected_end), label + ' period drifted from month-end')
        require(current.get('billing_cycle_anchor') == anchor, label + ' anchor changed')
        row[label] = {'start': target, 'end': expected_end, 'anchor': anchor}
        save()
    row['status'] = 'passed_calendar_control_only'
    save()


def verify_upgrade(provider, path):
    row = json.loads(Path(path).read_text())
    require(row.get('state') == 'active' and row.get('contract_id') and row.get('assignment_matches') is True and row.get('limits_match') is True,
            'Application activation/assignment snapshot is incomplete')
    require(row.get('anchor_preserved') is True and row.get('contract_starts_at_anchor') is True,
            'Application existing anchor/next contract start differs')
    require(row.get('contract_fee') == 25000 and row.get('contract_months') == 1, 'Recurring Pro terms differ')
    ids = {k: row.get(k) for k in ('customer_id', 'session_id', 'grant_id', 'subscription_id')}
    require(all(isinstance(v, str) and re.fullmatch(r'[A-Za-z0-9_]+', v) for v in ids.values()), 'Invalid evidence object IDs')
    require(isinstance(row.get('id'), str) and re.fullmatch(r'[0-9a-f-]{36}', row['id']), 'Invalid upgrade ID')
    session = provider.request('GET', '/v1/checkout/sessions/' + ids['session_id'])
    require(session.get('customer') == ids['customer_id'] and session.get('mode') == 'payment'
            and session.get('status') == 'complete' and session.get('metadata', {}).get('pro_upgrade_id') == row['id'],
            'One-off session association differs')
    require(session.get('amount_subtotal') == row.get('amount_cents') and session.get('currency') == 'usd'
            and session.get('payment_status') in ('paid', 'no_payment_required'), 'One-off payment quote/status differs')
    invoice_id = session.get('invoice')
    require(isinstance(invoice_id, str) and re.fullmatch(r'in_[A-Za-z0-9_]+', invoice_id), 'One-off invoice missing')
    invoice = provider.request('GET', '/v1/invoices/' + invoice_id)
    require(invoice.get('customer') == ids['customer_id'] and invoice.get('status') == 'paid'
            and invoice.get('amount_paid') == session.get('amount_total') and invoice.get('amount_remaining') == 0
            and not invoice.get('paid_out_of_band') and not invoice.get('starting_balance')
            and not invoice.get('ending_balance'), 'One-off invoice payment differs')
    if session.get('amount_total', 0) > 0:
        intent_id = session.get('payment_intent')
        require(isinstance(intent_id, str) and re.fullmatch(r'pi_[A-Za-z0-9_]+', intent_id), 'Payment intent missing')
        intent = provider.request('GET', '/v1/payment_intents/' + intent_id)
        require(intent.get('customer') == ids['customer_id'] and intent.get('status') == 'succeeded'
                and intent.get('amount_received') == session['amount_total'], 'Cash payment intent differs')
        charge_id = intent.get('latest_charge')
        require(isinstance(charge_id, str) and re.fullmatch(r'ch_[A-Za-z0-9_]+', charge_id), 'Charge missing')
        charge = provider.request('GET', '/v1/charges/' + charge_id)
        require(charge.get('customer') == ids['customer_id'] and charge.get('payment_intent') == intent_id
                and charge.get('paid') is True and charge.get('captured') is True
                and not charge.get('refunded') and charge.get('amount_refunded') == 0
                and not charge.get('disputed'), 'Cash charge differs')
    grant = provider.request('GET', '/v1/billing/credit_grants/' + ids['grant_id'])
    require(grant.get('customer') == ids['customer_id'] and grant.get('category') == 'paid'
            and not grant.get('expires_at') and not grant.get('voided_at')
            and grant.get('amount', {}).get('monetary') == {'currency': 'usd', 'value': row['amount_cents']}
            and grant.get('metadata', {}).get('pro_upgrade_id') == row['id']
            and grant.get('amount', {}).get('type') == 'monetary' and grant.get('priority') == 50
            and grant.get('applicability_config', {}).get('scope', {}).get('price_type') == 'metered'
            and not grant.get('applicability_config', {}).get('scope', {}).get('prices'),
            'Matching non-expiring paid credit differs')
    subscription = provider.request('GET', '/v1/subscriptions/' + ids['subscription_id'])
    require(subscription.get('customer') == ids['customer_id'] and not subscription.get('discounts'),
            'Subscription customer or recurring promotion differs')
    return {'upgrade_id': row['id'], 'status': 'verified_active_snapshot_and_provider_objects',
            'first_full_fee_worker_evidence': 'not_proven_by_this_check'}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--state-dir', type=Path, default=Path('/var/lib/superserve-devbox'))
    parser.add_argument('--expected-account', required=True)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--run-test-calendar', action='store_true')
    parser.add_argument('--upgrade-json', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    result = {'status': 'blocked', 'requests': [], 'calendars': [], 'verified_test_mode': False}

    def save():
        args.output.write_text(json.dumps(result, indent=2) + '\n')

    try:
        require(os.geteuid() == 0, 'Run on-box as root')
        require(not args.output.exists(), 'Use a new evidence output file for each run')
        spec = importlib.util.spec_from_file_location('devbox_services', Path(__file__).parent / 'services/devbox_services.py')
        helpers = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helpers)
        config = helpers.read_env('/etc/superserve-devbox/devbox.env')
        require(config.get('DEVBOX_NAME') == 'awille-ai-dev-billing' and config.get('DEVBOX_ID') == '29baf003ad05',
                'Wrong isolated integration box')
        binding = json.loads((args.state_dir / 'pricing-stripe-binding.json').read_text())
        require(binding.get('verified_test_mode') is True and binding.get('account_id') == args.expected_account,
                'RC verified binding receipt required before provider calls')
        env = helpers.read_env(args.state_dir / 'api.env')
        provider = Provider(env.get('STRIPE_SECRET_KEY', ''), env.get('STRIPE_API_VERSION', ''), args.expected_account,
                            result['requests'])
        provider.bind()
        result.update(verified_test_mode=True, account_id=args.expected_account, api_version=provider.version)
        if args.run_test_calendar:
            run_id = uuid.uuid4().hex
            result['run_id'] = run_id
            for year in (2027, 2028):
                run_calendar(provider, year, run_id, result, save)
        if args.upgrade_json:
            result['upgrade'] = verify_upgrade(provider, args.upgrade_json)
        result['status'] = 'passed_requested_scope_only'
        save()
        print('PASS requested test-mode scope; root-only receipt: ' + str(args.output))
        return 0
    except (QAError, OSError, ValueError, TypeError, KeyError):
        import sys
        error = sys.exc_info()[1]
        result['status'] = 'blocked'
        result['error'] = str(error) if isinstance(error, QAError) else 'Local configuration or receipt invalid; inspect on-box'
        if not args.output.exists():
            save()
        elif result.get('account_id'):
            save()
        print(result['error'], file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
