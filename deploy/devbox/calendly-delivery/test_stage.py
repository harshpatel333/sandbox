import importlib.util
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location('stage', Path(__file__).with_name('stage_github_secrets.py'))
stage = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage)


class DeliveryTest(unittest.TestCase):
    def test_preflight_never_writes(self):
        result = stage.stage({key: 'synthetic-' + key for key in stage.KEYS}, run=lambda *args, **kwargs: self.fail('unexpected invocation'))
        self.assertEqual(result['status'], 'READY_NOT_APPLIED')

    def test_invalid_batch_prevents_any_write(self):
        for missing in stage.KEYS:
            env = {key: 'synthetic' for key in stage.KEYS if key != missing}
            result = stage.stage(env, apply=True, run=lambda *args, **kwargs: self.fail('unexpected invocation'))
            self.assertEqual(result['status'], 'INPUT_REFUSED_NO_WRITES')

    def test_only_stdin_contains_values_and_partial_write_stops(self):
        calls = []
        values = {key: 'sentinel-' + key for key in stage.KEYS}

        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            return subprocess.CompletedProcess(argv, 0 if len(calls) == 1 else 1)

        result = stage.stage(values, apply=True, run=run)
        self.assertEqual(result['status'], 'PARTIAL_OR_AMBIGUOUS_STOP_NO_AUTOMATIC_RETRY')
        self.assertEqual(len(calls), 2)
        for argv, kwargs in calls:
            for value in values.values():
                self.assertNotIn(value, ' '.join(argv))
                self.assertNotIn(value, str(result))
            self.assertIn(kwargs['input'].decode(), values.values())
        self.assertEqual(len(result['written_names']), 1)


if __name__ == '__main__':
    unittest.main()
