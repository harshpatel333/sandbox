import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import Mock
import urllib.error

spec = importlib.util.spec_from_file_location('pro_qa', Path(__file__).with_name('pricing-pro-provider-qa.py'))
qa = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qa)


class Response(io.BytesIO):
    status = 200
    headers = {'Request-Id': 'req_fixture'}

    def __init__(self, value):
        super().__init__(json.dumps(value).encode())


class ProviderSafetyTests(unittest.TestCase):
    def provider(self):
        provider = qa.Provider('rk_test_fixture', '2026-08-26.dahlia', 'acct_fixture', [])
        provider.opener = Mock()
        return provider

    def test_live_key_is_rejected_before_network(self):
        with self.assertRaisesRegex(qa.QAError, 'Test-mode credential required'):
            qa.Provider('sk_live_fixture', 'version', 'acct_fixture', [])

    def test_mutation_is_blocked_before_account_and_balance(self):
        provider = self.provider()
        with self.assertRaisesRegex(qa.QAError, 'Account binding'):
            provider.request('POST', '/v1/customers', {}, 'fixture')
        provider.opener.open.assert_not_called()

    def test_wrong_account_or_live_balance_never_opens_mutations(self):
        for account, balance in [({'id': 'acct_other'}, {'livemode': False}),
                                 ({'id': 'acct_fixture'}, {'livemode': True})]:
            with self.subTest(account=account, balance=balance):
                provider = self.provider()
                provider.opener.open.side_effect = [Response(account), Response(balance)]
                with self.assertRaises(qa.QAError):
                    provider.bind()
                self.assertFalse(provider.bound)
                self.assertTrue(all(call.args[0].method == 'GET' for call in provider.opener.open.call_args_list))

    def test_http_failure_reports_capability_without_response_or_credential(self):
        provider = self.provider()
        provider.opener.open.side_effect = urllib.error.HTTPError('https://api.stripe.com/v1/account', 403,
                  'sensitive body', {'Request-Id': 'req_fixture'}, io.BytesIO(b'rk_test_fixture sensitive'))
        with self.assertRaises(qa.QAError) as error:
            provider.bind()
        self.assertNotIn('rk_test_fixture', str(error.exception))
        self.assertNotIn('sensitive', str(error.exception))
        self.assertEqual(provider.evidence[0]['status'], 403)
        self.assertEqual(provider.evidence[0]['path'], '/v1/account')

    def test_redirect_cannot_forward_authorization(self):
        self.assertIsNone(qa.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://example.com'))

    def test_subscription_create_cannot_be_run_without_idempotency(self):
        provider = self.provider()
        provider.bound = True
        with self.assertRaisesRegex(qa.QAError, 'idempotency'):
            provider.request('POST', '/v1/subscriptions', {})
        provider.opener.open.assert_not_called()


class CalendarTests(unittest.TestCase):
    def test_both_february_lengths_and_original_anchor_are_asserted(self):
        for year, day in [(2027, 28), (2028, 29)]:
            with self.subTest(year=year):
                start = qa.timestamp(year, 1, 31)
                end = qa.timestamp(year, 2, day)
                march = qa.timestamp(year, 3, 31)
                state = {'start': start, 'end': end, 'time': start, 'hold': None}
                calls = []

                def request(method, path, params=None, operation=None):
                    calls.append((method, path, params))
                    if method == 'POST' and path.endswith('/advance'):
                        state['time'] = params['frozen_time']
                        state['start'] = state['time']
                        state['end'] = march if state['time'] == end else qa.timestamp(year, 4, 30)
                    if path == '/v1/subscriptions/sub_fixture' and method == 'POST':
                        state['hold'] = {'behavior': 'keep_as_draft'} if 'pause_collection[behavior]' in params else None
                    if path.startswith('/v1/test_helpers/test_clocks'):
                        return {'id': 'clock_fixture', 'livemode': False, 'status': 'ready', 'frozen_time': state['time']}
                    if path.startswith('/v1/subscriptions'):
                        return {'id': 'sub_fixture', 'billing_cycle_anchor': start, 'pause_collection': state['hold'],
                                'items': {'data': [{'current_period_start': state['start'], 'current_period_end': state['end']}]}}
                    return {'id': 'object_fixture'}

                provider = Mock()
                provider.request.side_effect = request
                result = {'calendars': []}
                qa.run_calendar(provider, year, 'a' * 32, result, lambda: None)
                self.assertEqual(result['calendars'][0]['status'], 'passed_calendar_control_only')
                self.assertEqual(result['calendars'][0]['expected_boundaries'], [start, end, march])
                create = next(params for method, path, params in calls if method == 'POST' and path == '/v1/subscriptions')
                self.assertEqual(create['collection_method'], 'send_invoice')
                self.assertNotIn('pause_collection[behavior]', create)
                customer = next(params for method, path, params in calls if method == 'POST' and path == '/v1/customers')
                self.assertEqual(customer['email'], qa.fixture_email('a' * 32, year))


if __name__ == '__main__':
    unittest.main()
