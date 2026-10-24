import importlib.util
import io
import json
from pathlib import Path
import time
import unittest
from unittest.mock import Mock
import urllib.error

spec = importlib.util.spec_from_file_location('diagnostic', Path(__file__).with_name('pricing-pro-provider-diagnostic.py'))
diag = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diag)


def source():
    return {'status': 'blocked', 'verified_test_mode': True, 'account_id': 'acct_fixture',
            'api_version': '2026-05-27.dahlia', 'run_id': 'a' * 32,
            'calendars': [{'year': 2027, 'status': 'pending', 'clock': 'clock_fixture', 'customer': 'cus_fixture',
                           'product': 'prod_fixture', 'price': 'price_fixture'}],
            'requests': [{'method': 'POST', 'path': '/v1/subscriptions', 'status': 400, 'request_id': 'req_original'}]}


class DiagnosticTests(unittest.TestCase):
    def test_exact_original_form_and_idempotency_are_reconstructed(self):
        row, fields, key = diag.failed_request(source(), 'acct_fixture')
        self.assertEqual(key, 'dev-pro-qa:' + 'a' * 32 + ':2027:subscription')
        self.assertEqual(fields, {'customer': 'cus_fixture', 'items[0][price]': 'price_fixture',
                         'collection_method': 'send_invoice', 'days_until_due': 30, 'proration_behavior': 'none',
                         'billing_mode[type]': 'flexible', 'metadata[dev_pro_qa]': 'a' * 32})

    def test_other_failure_stage_cannot_be_replayed(self):
        for key, value in [('api_version', '2026-07-29.dahlia'), ('account_id', 'acct_other'), ('status', 'passed')]:
            s = source()
            s[key] = value
            with self.subTest(key=key), self.assertRaises(diag.DiagnosticError):
                diag.failed_request(s, 'acct_fixture')

    def test_error_whitelist_redacts_credential_and_ignores_full_body(self):
        body = json.dumps({'error': {'type': 'invalid_request_error', 'code': 'rk_test_fixture',
                   'param': 'billing_mode[type]', 'message': 'bad rk_test_fixture https://example.com user@example.com',
                   'payment_intent': {'secret': 'hidden'}}, 'secret': 'hidden'})
        result = diag.error_fields(body, 'rk_test_fixture')
        self.assertEqual(set(result), {'type', 'code', 'param', 'message'})
        self.assertNotIn('rk_test_fixture', json.dumps(result))
        self.assertNotIn('hidden', json.dumps(result))
        self.assertNotIn('user@example.com', json.dumps(result))
        self.assertEqual(result['param'], 'billing_mode[type]')

    def test_mutation_before_binding_is_blocked(self):
        client = diag.Client('rk_test_fixture', '2026-05-27.dahlia', {'requests': []}, lambda: None)
        client.opener = Mock()
        with self.assertRaises(diag.DiagnosticError):
            client.request('POST', '/v1/subscriptions', {}, 'original')
        client.opener.open.assert_not_called()

    def test_only_subscription_mutation_permitted(self):
        client = diag.Client('rk_test_fixture', '2026-05-27.dahlia', {'requests': []}, lambda: None)
        client.bound = True
        client.opener = Mock()
        with self.assertRaises(diag.DiagnosticError):
            client.request('POST', '/v1/customers', {}, 'new')
        client.opener.open.assert_not_called()

    def test_http400_keeps_request_identity_and_sanitized_error(self):
        result = {'requests': []}
        client = diag.Client('rk_test_fixture', '2026-05-27.dahlia', result, lambda: None)
        client.bound = True
        client.opener = Mock()
        client.opener.open.side_effect = urllib.error.HTTPError('url', 400, 'raw', {'Request-Id': 'req_replay'},
            io.BytesIO(json.dumps({'error': {'type': 'invalid_request_error', 'code': 'parameter_unknown',
                              'param': 'billing_mode[type]', 'message': 'Unknown parameter'}}).encode()))
        with self.assertRaises(diag.DiagnosticError):
            client.request('POST', '/v1/subscriptions', {}, 'original')
        self.assertEqual(result['requests'][0]['request_id'], 'req_replay')
        self.assertEqual(result['requests'][0]['error']['param'], 'billing_mode[type]')

    def test_existing_or_paginated_customer_inventory_blocks_replay(self):
        row, fields, _ = diag.failed_request(source(), 'acct_fixture')
        for subscriptions in [{'has_more': True, 'data': []}, {'has_more': False, 'data': [{'id': 'sub_existing'}]}]:
            with self.subTest(subscriptions=subscriptions):
                client = Mock()
                client.request.side_effect = [{'id': 'cus_fixture', 'test_clock': 'clock_fixture'},
                    {'id': 'prod_fixture'}, {'id': 'price_fixture', 'product': 'prod_fixture', 'unit_amount': 25000,
                    'currency': 'usd', 'recurring': {'interval': 'month'}},
                    {'id': 'clock_fixture', 'status': 'ready', 'frozen_time': 1801396800, 'created': int(time.time())},
                    subscriptions]
                with self.assertRaises(diag.DiagnosticError):
                    diag.inventory(client, row, fields, {})
                self.assertTrue(all(call.args[0] == 'GET' for call in client.request.call_args_list))


if __name__ == '__main__':
    unittest.main()
