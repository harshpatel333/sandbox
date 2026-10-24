import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock

spec = importlib.util.spec_from_file_location('calendar_resume', Path(__file__).with_name('pricing-pro-calendar-resume.py'))
resume = importlib.util.module_from_spec(spec)
spec.loader.exec_module(resume)


class ResumeTests(unittest.TestCase):
    def row(self):
        return {'year': 2027, 'status': 'pending', 'clock': 'clock_fixture', 'customer': 'cus_fixture',
                'product': 'prod_fixture', 'price': 'price_fixture'}

    def customer(self, email):
        return {'id': 'cus_fixture', 'test_clock': 'clock_fixture', 'name': 'dev-pro-qa:' + 'a' * 32 + ':2027', 'email': email}

    def test_missing_email_changes_only_email_on_original_customer(self):
        provider = Mock()
        email = resume.qa.fixture_email('a' * 32, 2027)
        provider.request.side_effect = [self.customer(None), {'id': 'prod_fixture', 'name': self.customer(None)['name']},
                                        self.customer(email), self.customer(email)]
        resume.repair_customer(provider, self.row(), 'a' * 32, lambda: None)
        writes = [call for call in provider.request.call_args_list if call.args[0] == 'POST']
        self.assertEqual(len(writes), 1)
        self.assertEqual(writes[0].args, ('POST', '/v1/customers/cus_fixture', {'email': email},
                         'dev-pro-qa:' + 'a' * 32 + ':2027:customer-email'))

    def test_recognized_email_replay_does_not_update_customer(self):
        provider = Mock()
        email = resume.qa.fixture_email('a' * 32, 2027)
        provider.request.side_effect = [self.customer(email), {'id': 'prod_fixture', 'name': self.customer(email)['name']},
                                        self.customer(email)]
        resume.repair_customer(provider, self.row(), 'a' * 32, lambda: None)
        self.assertTrue(all(call.args[0] == 'GET' for call in provider.request.call_args_list))

    def test_nonempty_unknown_email_is_not_clobbered(self):
        provider = Mock()
        provider.request.side_effect = [self.customer('other@example.com'),
                                       {'id': 'prod_fixture', 'name': self.customer(None)['name']}]
        with self.assertRaisesRegex(resume.qa.QAError, 'Nonempty unrecognized'):
            resume.repair_customer(provider, self.row(), 'a' * 32, lambda: None)
        self.assertTrue(all(call.args[0] == 'GET' for call in provider.request.call_args_list))

    def test_unowned_customer_is_never_updated(self):
        provider = Mock()
        customer = self.customer(None)
        customer['name'] = 'other-fixture'
        provider.request.side_effect = [customer, {'id': 'prod_fixture', 'name': self.customer(None)['name']}]
        with self.assertRaisesRegex(resume.qa.QAError, 'ownership'):
            resume.repair_customer(provider, self.row(), 'a' * 32, lambda: None)
        self.assertTrue(all(call.args[0] == 'GET' for call in provider.request.call_args_list))

    def test_original_calendar_reuses_objects_and_exact_subscription_key(self):
        qa = resume.qa
        start, feb, march = qa.timestamp(2027, 1, 31), qa.timestamp(2027, 2, 28), qa.timestamp(2027, 3, 31)
        state = {'time': start, 'end': feb, 'hold': None}
        calls = []

        def request(method, path, fields=None, key=None):
            calls.append((method, path, fields, key))
            if path.endswith('/advance'):
                state['time'] = fields['frozen_time']
                state['end'] = march if state['time'] == feb else qa.timestamp(2027, 4, 30)
            if path == '/v1/subscriptions/sub_fixture' and method == 'POST':
                state['hold'] = {'behavior': 'keep_as_draft'} if 'pause_collection[behavior]' in fields else None
            if path.startswith('/v1/test_helpers/test_clocks'):
                return {'status': 'ready', 'frozen_time': state['time']}
            return {'id': 'sub_fixture', 'billing_cycle_anchor': start, 'pause_collection': state['hold'],
                    'items': {'data': [{'current_period_start': state['time'], 'current_period_end': state['end']}]}}

        provider = Mock()
        provider.request.side_effect = request
        result = {'calendars': []}
        qa.run_calendar(provider, 2027, 'a' * 32, result, lambda: None, existing=self.row())
        forbidden = ['/v1/customers', '/v1/products', '/v1/prices', '/v1/test_helpers/test_clocks']
        self.assertFalse(any(method == 'POST' and path in forbidden for method, path, _, _ in calls))
        subscription = next(call for call in calls if call[1] == '/v1/subscriptions')
        self.assertEqual(subscription[3], 'dev-pro-qa:' + 'a' * 32 + ':2027:subscription')
        self.assertEqual(subscription[2]['customer'], 'cus_fixture')
        self.assertEqual(subscription[2]['items[0][price]'], 'price_fixture')
        self.assertEqual(result['calendars'][0]['clock'], 'clock_fixture')
        self.assertEqual(result['calendars'][0]['status'], 'passed_calendar_control_only')


if __name__ == '__main__':
    unittest.main()
