#!/usr/bin/env python3
import json
import os
import subprocess
import sys

KEYS = ('CALENDLY_API_TOKEN', 'CALENDLY_ORGANIZATION_URI', 'CALENDLY_ENTERPRISE_EVENT_TYPE_URI')


def stage(env, apply=False, run=subprocess.run):
    values = {key: env.get(key, '') for key in KEYS}
    if any(not value or any(c in value for c in '\r\n\x00') for value in values.values()):
        return {'status': 'INPUT_REFUSED_NO_WRITES', 'written_names': []}
    if not apply:
        return {'status': 'READY_NOT_APPLIED', 'present': {key: True for key in KEYS}}
    written = []
    for key in KEYS:
        try:
            result = run(['gh', 'secret', 'set', 'DEVBOX_SECRET_' + key, '--repo', 'superserve-ai/sandbox', '--env', 'devbox'], input=values[key].encode(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
            if result.returncode != 0:
                raise ValueError()
        except BaseException:
            return {'status': 'PARTIAL_OR_AMBIGUOUS_STOP_NO_AUTOMATIC_RETRY', 'written_names': written, 'attempted_name': 'DEVBOX_SECRET_' + key}
        written.append('DEVBOX_SECRET_' + key)
    return {'status': 'GITHUB_SECRET_WRITES_ACKNOWLEDGED_NOT_RUNTIME_VERIFIED', 'written_names': written}


def main():
    if sys.argv[1:] not in ([], ['--apply-github-secrets']):
        print(json.dumps({'status': 'INPUT_REFUSED_NO_WRITES'}))
        return 1
    result = stage(os.environ, apply=bool(sys.argv[1:]))
    print(json.dumps(result))
    return 0 if result['status'] in ('READY_NOT_APPLIED', 'GITHUB_SECRET_WRITES_ACKNOWLEDGED_NOT_RUNTIME_VERIFIED') else 1


if __name__ == '__main__':
    raise SystemExit(main())
