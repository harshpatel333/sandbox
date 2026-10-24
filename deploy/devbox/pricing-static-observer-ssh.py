#!/usr/bin/env python3
import argparse
import base64
import hashlib
import fcntl
import json
import os
import re
from pathlib import Path
import shlex
import stat
import subprocess

LEAF_SHA256 = '1deee84a5e77170f8c5d9eeb877a867a8083e6876d6f8e68c6de99123daebb80'
BOOTSTRAP = "import base64,hashlib,json,sys; p=json.loads(sys.stdin.buffer.read(131073)); s=base64.b64decode(p['source'],validate=True); assert hashlib.sha256(s).hexdigest()=='" + LEAF_SHA256 + "'; sys.argv=['pricing-static-observer.py',*p['args']]; exec(compile(s,'pricing-static-observer.py','exec'),{'__name__':'__main__'})"
ARGUMENTS = ('devbox-name', 'devbox-id', 'prior-receipt', 'prior-sha256', 'backend-sha', 'console-sha', 'binary-sha256', 'schema-sha256', 'epoch', 'compose-file', 'compose-sha256')
RUNTIME_KEYS = ('backend_sha', 'console_sha', 'pid', 'binary_sha256', 'console_container_id', 'schema_receipt_sha256', 'epoch', 'pricing_version', 'entry_open')


def private_bytes(path, digest):
    if not isinstance(digest, str) or not re.fullmatch('[a-f0-9]{64}', digest):
        raise ValueError()
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise ValueError()
    value = path.read_bytes()
    if len(value) > 131072 or hashlib.sha256(value).hexdigest() != digest:
        raise ValueError()
    return value


def failure(value):
    if value is None:
        return None
    if not isinstance(value, dict):
        raise ValueError()
    stages = {'input.args', 'input.prior', 'input.target', 'runtime.before', 'runtime.after', 'static.launch', 'static.stdin_parse', 'static.process_env', 'static.asset_directory', 'static.assets_scan', 'static.serialize'}
    classes = {'PermissionError', 'FileNotFoundError', 'TimeoutError', 'ValueError', 'SyntaxError', 'TypeError', 'RangeError', 'CalledProcessError', 'OSError'}
    if value.get('stage') not in stages or value.get('exception_class') not in classes:
        raise ValueError()
    for key, low, high in (('errno', 1, 4095), ('status', -128, 255)):
        number = value.get(key)
        if number is not None and (type(number) is not int or not low <= number <= high):
            raise ValueError()
    return {key: value.get(key) for key in ('stage', 'exception_class', 'errno', 'status')}


def sanitized(value, pins):
    if not isinstance(value, dict) or value.get('status') not in ('STATIC_OBSERVATION_COMPLETE', 'PARTIAL_OR_FAILED_STATIC_OBSERVATION'):
        raise ValueError()
    result = {'status': value['status'], 'primary_failure': failure(value.get('primary_failure')), 'after_failure': failure(value.get('after_failure')), 'static_presence': None, 'runtime_before': None, 'runtime_after': None, 'runtime_unchanged': None}
    for key in ('runtime_before', 'runtime_after'):
        item = value.get(key)
        if item is not None:
            if not isinstance(item, dict) or any(item.get(name.replace('-', '_')) != pins[name] for name in ('backend-sha', 'console-sha', 'binary-sha256', 'epoch')):
                raise ValueError()
            for name in ('console_container_id', 'schema_receipt_sha256'):
                if not isinstance(item.get(name), str) or len(item[name]) != 64 or any(c not in '0123456789abcdef' for c in item[name]):
                    raise ValueError()
            if item['schema_receipt_sha256'] != pins['schema-sha256']:
                raise ValueError()
            if type(item.get('pid')) is not int or item['pid'] <= 1 or item.get('pricing_version') != 'v2' or item.get('entry_open') is not True:
                raise ValueError()
            result[key] = {name: item[name] for name in RUNTIME_KEYS}
    present = value.get('static_presence')
    if present is not None:
        result['static_presence'] = {}
        groups = {'compiled_asset_presence': ('recaptcha_script_code', 'turnstile_script_code', 'fingerprint_client_code', 'recaptcha_candidate_key_literal', 'turnstile_candidate_key_literal', 'fingerprint_candidate_key_literal'), 'running_server_env_presence': ('recaptcha_api_key', 'recaptcha_project_id', 'recaptcha_site_key', 'turnstile_site_key', 'fingerprint_api_key')}
        for group, keys in groups.items():
            item = present.get(group, {})
            if any(name not in item or (type(item[name]) is not bool and not (name.endswith('_candidate_key_literal') and item[name] is None)) for name in keys):
                raise ValueError()
            result['static_presence'][group] = {name: item[name] for name in keys}
    if result['runtime_before'] is not None and result['runtime_after'] is not None:
        result['runtime_unchanged'] = result['runtime_before'] == result['runtime_after']
    if value['status'] == 'STATIC_OBSERVATION_COMPLETE' and (result['primary_failure'] is not None or result['after_failure'] is not None or result['static_presence'] is None or result['runtime_unchanged'] is not True):
        raise ValueError()
    prior = value.get('prior_observations')
    result['prior_observations'] = None
    if prior is not None:
        if prior.get('source_run') != 40122720783 or prior.get('original_run') != 40121793821:
            raise ValueError()
        if not isinstance(prior.get('observed_at'), str) or not re.fullmatch(r'[0-9T:.+Z-]{20,40}', prior['observed_at']):
            raise ValueError()
        kept = {'source_run': 40122720783, 'original_run': 40121793821, 'observed_at': prior['observed_at'], 'counts': {}, 'mailpit_counts': {}}
        for group, keys in (('counts', ('auth_users', 'confirmed_auth_users', 'profiles', 'owner_teams', 'current_assignments')), ('mailpit_counts', ('matching_recipient_messages', 'confirmation_subject_messages'))):
            for name in keys:
                number = prior.get(group, {}).get(name)
                if type(number) is not int or number != 0:
                    raise ValueError()
                kept[group][name] = number
        if prior.get('mailpit_counts', {}).get('counts_complete') is not True:
            raise ValueError()
        kept['mailpit_counts']['counts_complete'] = True
        kept['prior_inspection_complete'] = False
        result['prior_observations'] = kept
    if value['status'] == 'STATIC_OBSERVATION_COMPLETE' and result['prior_observations'] is None:
        raise ValueError()
    result['sdk_readiness'] = 'UNAVAILABLE_NO_BROWSER_OR_SDK_PROBE'
    result['root_cause'] = 'NOT_ESTABLISHED'
    result['counts_requeried'] = False
    return result


def execute(args):
    leaf = Path(__file__).with_name('pricing-static-observer.py').read_bytes()
    if hashlib.sha256(leaf).hexdigest() != LEAF_SHA256:
        raise ValueError()
    config = Path(args.ssh_config).resolve(strict=True)
    config_bytes = private_bytes(config, args.ssh_config_sha256)
    pins = json.loads(private_bytes(Path(args.pins), args.pins_sha256))
    if set(pins) != set(ARGUMENTS) or any(not isinstance(value, str) or '\x00' in value or '\n' in value for value in pins.values()):
        raise ValueError()
    if pins['backend-sha'] != '4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b' or pins['console-sha'] != '587039f5e5bd9994e4e2f52a7c6a2e5daf452984':
        raise ValueError()
    if pins['devbox-name'] != 'awille-ai-dev-billing' or pins['devbox-id'] != '29baf003ad05':
        raise ValueError()
    output_fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    config_fd = None
    result = {'status': 'CALLER_NOT_EXECUTED', 'leaf_sha256': LEAF_SHA256, 'actual_static_result': None}
    try:
        once_fd = os.open(args.once_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.close(once_fd)
        config_fd = os.memfd_create('reviewed-ssh-config', os.MFD_ALLOW_SEALING)
        os.write(config_fd, config_bytes)
        fcntl.fcntl(config_fd, fcntl.F_ADD_SEALS, fcntl.F_SEAL_WRITE | fcntl.F_SEAL_GROW | fcntl.F_SEAL_SHRINK | fcntl.F_SEAL_SEAL)
        payload = json.dumps({'source': base64.b64encode(leaf).decode(), 'args': [part for name in ARGUMENTS for part in ('--' + name, pins[name])]}).encode()
        remote = 'python3 -I -c ' + shlex.quote(BOOTSTRAP)
        command = ['/usr/bin/ssh', '-F', '/proc/self/fd/' + str(config_fd), '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'UpdateHostKeys=no', '-o', 'PermitLocalCommand=no', '-o', 'ClearAllForwardings=yes', '-o', 'ForwardAgent=no', '-o', 'ForwardX11=no', '-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ConnectionAttempts=1', '-o', 'ConnectTimeout=15', 'rc-billing-readonly', remote]
        completed = subprocess.run(command, input=payload, capture_output=True, timeout=80, pass_fds=(config_fd,))
        if len(completed.stdout) > 65536 or completed.returncode not in (0, 1):
            raise ValueError()
        observed = sanitized(json.loads(completed.stdout), pins)
        if (completed.returncode == 0) != (observed['status'] == 'STATIC_OBSERVATION_COMPLETE'):
            raise ValueError()
        result.update(status='STATIC_OBSERVATION_RECORDED', actual_static_result=observed)
    except Exception as error:
        result.update(status='CALLER_FAILED_OR_AMBIGUOUS_NO_RETRY', failure={'stage': 'caller.ssh_or_readback', 'exception_class': 'TimeoutError' if isinstance(error, subprocess.TimeoutExpired) else 'OSError', 'status': None})
    finally:
        if config_fd is not None:
            os.close(config_fd)
        with os.fdopen(output_fd, 'w') as stream:
            json.dump(result, stream, indent=2)
            stream.write('\n')
    return result


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError()


def main():
    parser = SafeParser(add_help=False)
    for name in ('ssh-config', 'ssh-config-sha256', 'pins', 'pins-sha256', 'once-file', 'output'):
        parser.add_argument('--' + name, required=True)
    try:
        args = parser.parse_args()
        result = execute(args)
    except Exception:
        print(json.dumps({'status': 'CALLER_FAILED_EXECUTION_STATE_UNAVAILABLE_NO_RETRY'}))
        return 1
    print(json.dumps(result))
    return 0 if result['status'] == 'STATIC_OBSERVATION_RECORDED' else 1


if __name__ == '__main__':
    raise SystemExit(main())
