#!/usr/bin/env python3
import argparse
import datetime
import hashlib
import json
import multiprocessing
import re
import shlex
import subprocess
import time
from pathlib import Path


STAGES = {
    'input.args', 'input.prior', 'input.target', 'runtime.before', 'runtime.after',
    'static.launch', 'static.stdin_parse', 'static.process_env',
    'static.asset_directory', 'static.assets_scan', 'static.serialize',
}
CLASSES = {
    'PermissionError', 'FileNotFoundError', 'TimeoutError', 'ValueError',
    'SyntaxError', 'TypeError', 'RangeError', 'CalledProcessError', 'OSError',
}
STATIC_NODE = r'''
const fs=require('node:fs'),path=require('node:path');
let stage='static.stdin_parse';
try {
  const candidate=JSON.parse(fs.readFileSync(0,'utf8'));
  stage='static.process_env';
  const env=Object.fromEntries(fs.readFileSync('/proc/1/environ','utf8').split('\0').filter(x=>x.includes('=')).map(x=>{const i=x.indexOf('=');return [x.slice(0,i),x.slice(i+1)]}));
  const found={recaptcha_script_code:false,turnstile_script_code:false,fingerprint_client_code:false,recaptcha_candidate_key_literal:null,turnstile_candidate_key_literal:null,fingerprint_candidate_key_literal:null};
  for(const key of ['recaptcha','turnstile','fingerprint'])if(candidate[key])found[key+'_candidate_key_literal']=false;
  stage='static.asset_directory';
  const base='/src/apps/console/.next/static';
  fs.readdirSync(base);
  stage='static.assets_scan';
  const deadline=Date.now()+10000;
  let files=0,bytes=0;
  function scan(dir){
    for(const e of fs.readdirSync(dir,{withFileTypes:true})){
      if(Date.now()>deadline||files>=5000||bytes>67108864)throw new RangeError();
      const f=path.join(dir,e.name);
      if(e.isDirectory())scan(f);
      else if(e.isFile()&&e.name.endsWith('.js')){
        const size=fs.statSync(f).size;
        if(size>8388608||bytes+size>67108864)throw new RangeError();
        const data=fs.readFileSync(f,'utf8');files++;bytes+=size;
        found.recaptcha_script_code ||= data.includes('google.com/recaptcha/enterprise.js');
        found.turnstile_script_code ||= data.includes('challenges.cloudflare.com/turnstile/v0/api.js');
        found.fingerprint_client_code ||= /fingerprint|Fingerprint/.test(data);
        for(const key of ['recaptcha','turnstile','fingerprint'])if(candidate[key])found[key+'_candidate_key_literal'] ||= data.includes(candidate[key]);
      }
    }
  }
  scan(base);
  if(files===0)throw new RangeError();
  stage='static.serialize';
  process.stdout.write(JSON.stringify({ok:true,compiled_asset_presence:found,running_server_env_presence:{recaptcha_api_key:Boolean(env.RECAPTCHA_API_KEY),recaptcha_project_id:Boolean(env.RECAPTCHA_PROJECT_ID),recaptcha_site_key:Boolean(env.NEXT_PUBLIC_RECAPTCHA_SITE_KEY),turnstile_site_key:Boolean(env.NEXT_PUBLIC_CLOUDFLARE_TURNSTILE_SITE_KEY),fingerprint_api_key:Boolean(env.NEXT_PUBLIC_FINGERPRINT_API_KEY)}}));
} catch(e) {
  const code=e&&e.code;
  const allowed={EACCES:['PermissionError',13],EPERM:['PermissionError',1],ENOENT:['FileNotFoundError',2],ENOTDIR:['OSError',20],EIO:['OSError',5]};
  const named=['SyntaxError','TypeError','RangeError'].includes(e&&e.name)?e.name:'OSError';
  const [exception_class,errno]=allowed[code]||[named,null];
  process.stdout.write(JSON.stringify({ok:false,failure:{stage,exception_class,errno,status:null}}));
  process.exitCode=1;
}
'''


STATIC_WRAPPER = r'''
const fs=require('node:fs'),{spawnSync}=require('node:child_process');
try {
  const input=fs.readFileSync(0,'utf8');
  const r=spawnSync(process.execPath,['-e',process.argv[1]],{input,encoding:'utf8',timeout:15000,killSignal:'SIGKILL',maxBuffer:16384});
  if(r.error){
    process.stdout.write(JSON.stringify({ok:false,failure:{stage:'static.launch',exception_class:r.error.code==='ETIMEDOUT'?'TimeoutError':'OSError',errno:null,status:null}}));
    process.exitCode=1;
  } else {
    process.stdout.write(r.stdout||'');
    process.exitCode=Number.isInteger(r.status)?r.status:1;
  }
} catch(e) {
  process.stdout.write(JSON.stringify({ok:false,failure:{stage:'static.stdin_parse',exception_class:'OSError',errno:null,status:null}}));
  process.exitCode=1;
}
'''


class Failure(Exception):
    def __init__(self, evidence):
        super().__init__('sanitized observation failure')
        self.evidence = evidence


def classified(stage, error=None, status=None):
    name = type(error).__name__ if error is not None else 'CalledProcessError'
    if isinstance(error, subprocess.TimeoutExpired):
        name = 'TimeoutError'
    if name not in CLASSES:
        name = 'OSError'
    number = getattr(error, 'errno', None)
    return {
        'stage': stage, 'exception_class': name,
        'errno': number if type(number) is int and 0 < number < 4096 else None,
        'status': status if type(status) is int and -128 <= status <= 255 else None,
    }


def command(argv, stage, body=None, timeout=10):
    try:
        result = subprocess.run(argv, input=body, capture_output=True, text=True, timeout=timeout)
    except Exception as error:
        raise Failure(classified(stage, error)) from None
    if result.returncode:
        raise Failure(classified(stage, status=result.returncode))
    if len(result.stdout) > 1048576:
        raise Failure(classified(stage, ValueError()))
    return result.stdout.strip()


def bounded_text(file, limit=1048576):
    with Path(file).open('rb') as stream:
        value = stream.read(limit + 1)
    if len(value) > limit:
        raise ValueError()
    return value.decode('utf8')


def checked_sha(value):
    if not re.fullmatch('[a-f0-9]{40}', value):
        raise ValueError()
    return value


class Observer:
    def __init__(self, args):
        self.args = args
        self.config = {}
        self.target_validated = False

    def prior(self):
        raw = bounded_text(self.args.prior_receipt)
        if hashlib.sha256(raw.encode()).hexdigest() != self.args.prior_sha256:
            raise ValueError()
        prior = json.loads(raw)
        if prior.get('original_run') != 40121793821 or prior.get('fixture_run') != 'd646e99d023620f137b5':
            raise ValueError()
        result = {'source_run': 40122720783, 'original_run': 40121793821,
                  'observed_at': prior.get('started_at'), 'counts': {}, 'mailpit_counts': {}}
        if not isinstance(result['observed_at'], str) or not re.fullmatch(r'[0-9T:.+Z-]{20,40}', result['observed_at']):
            raise ValueError()
        for name in ('auth_users', 'confirmed_auth_users', 'profiles', 'owner_teams', 'current_assignments'):
            value = prior.get('counts', {}).get(name)
            if type(value) is not int or value < 0:
                raise ValueError()
            result['counts'][name] = value
        for name in ('matching_recipient_messages', 'confirmation_subject_messages'):
            value = prior.get('mailpit_counts', {}).get(name)
            if type(value) is not int or value < 0:
                raise ValueError()
            result['mailpit_counts'][name] = value
        if prior.get('mailpit_counts', {}).get('counts_complete') is not True:
            raise ValueError()
        result['mailpit_counts']['counts_complete'] = True
        result['prior_inspection_complete'] = False
        return result

    def validate_target(self):
        checked_sha(self.args.backend_sha)
        checked_sha(self.args.console_sha)
        for value in (self.args.binary_sha256, self.args.schema_sha256, self.args.prior_sha256):
            if not re.fullmatch('[a-f0-9]{64}', value):
                raise ValueError()
        if not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}', self.args.epoch):
            raise ValueError()
        for line in bounded_text('/etc/superserve-devbox/devbox.env').splitlines():
            if '=' not in line or line.lstrip().startswith('#'):
                continue
            key, value = line.split('=', 1)
            if key in ('DEVBOX_NAME', 'DEVBOX_ID', 'RECAPTCHA_SITE_KEY', 'CLOUDFLARE_TURNSTILE_SITE_KEY', 'FINGERPRINT_API_KEY'):
                values = shlex.split(value)
                self.config[key] = values[0] if len(values) == 1 else ''
        if not re.fullmatch('[a-z0-9-]{1,63}', self.args.devbox_name) or not re.fullmatch('[a-f0-9]{12}', self.args.devbox_id):
            raise ValueError()
        if self.config.get('DEVBOX_NAME') != self.args.devbox_name or self.config.get('DEVBOX_ID') != self.args.devbox_id:
            raise ValueError()
        if not Path(self.args.compose_file).is_absolute():
            raise ValueError()
        if not re.fullmatch('[a-f0-9]{64}', self.args.compose_sha256):
            raise ValueError()
        if hashlib.sha256(Path(self.args.compose_file).read_bytes()).hexdigest() != self.args.compose_sha256:
            raise ValueError()
        self.target_validated = True

    def runtime(self, stage):
        context = multiprocessing.get_context('fork')
        parent, child = context.Pipe(duplex=False)
        def observe():
            parent.close()
            try:
                child.send({'snapshot': self.runtime_inner(stage)})
            except Failure as error:
                child.send({'failure': error.evidence})
            except Exception as error:
                child.send({'failure': classified(stage, error)})
            finally:
                child.close()
        worker = context.Process(target=observe, daemon=True)
        worker.start()
        child.close()
        try:
            if not parent.poll(20):
                raise Failure(classified(stage, TimeoutError()))
            try:
                result = parent.recv()
            except EOFError:
                raise Failure(classified(stage, OSError())) from None
            if 'failure' in result:
                raise Failure(result['failure'])
            return result['snapshot']
        finally:
            if worker.is_alive():
                worker.kill()
            worker.join(timeout=1)
            parent.close()

    def runtime_inner(self, stage):
        deadline = time.monotonic() + 20
        def run(argv, body=None):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError()
            return command(argv, stage, body, min(10, remaining))
        pid = int(run(['systemctl', 'show', 'superserve-devbox-api.service', '--property=MainPID', '--value']))
        binary = Path(f'/proc/{pid}/exe')
        if pid <= 1 or binary.resolve() != Path(f'/opt/superserve-devbox/api/releases/{self.args.backend_sha}/controlplane'):
            raise ValueError()
        digest = hashlib.sha256()
        total = 0
        with binary.open('rb') as stream:
            while chunk := stream.read(1048576):
                total += len(chunk)
                if time.monotonic() > deadline or total > 268435456:
                    raise TimeoutError()
                digest.update(chunk)
        if digest.hexdigest() != self.args.binary_sha256:
            raise ValueError()
        container = json.loads(run(['docker', 'inspect', 'superserve-devbox-console']))[0]
        if not container['State']['Running'] or container['Config']['Labels'].get('superserve-devbox.ui-revision') != self.args.console_sha:
            raise ValueError()
        schema = Path('/var/lib/superserve-devbox') / ('pricing-e2e-' + self.args.backend_sha) / 'runtime.json'
        if hashlib.sha256(bounded_text(schema).encode()).hexdigest() != self.args.schema_sha256:
            raise ValueError()
        argv = ['docker', 'compose', '--project-name', 'superserve-devbox', '--file', self.args.compose_file,
                '--env-file', '/var/lib/superserve-devbox/services.env', 'exec', '-T', 'db', 'psql', '-XAtq',
                '-U', 'supabase_admin', '-d', 'postgres', '-p', '54322', '-v', 'ON_ERROR_STOP=1']
        public = json.loads(run(argv, "BEGIN READ ONLY; SET LOCAL statement_timeout='5s'; SELECT row_to_json(p) FROM billing_pricing_public_state() p; ROLLBACK;"))
        if public.get('pricing_version') != 'v2' or public.get('entry_open') is not True or public.get('epoch') != self.args.epoch:
            raise ValueError()
        container_id = container['Id']
        if not re.fullmatch('[a-f0-9]{64}', container_id):
            raise ValueError()
        return {'backend_sha': self.args.backend_sha, 'console_sha': self.args.console_sha, 'pid': pid,
                'binary_sha256': digest.hexdigest(), 'console_container_id': container_id,
                'schema_receipt_sha256': self.args.schema_sha256, 'epoch': public['epoch'],
                'pricing_version': 'v2', 'entry_open': True}

    def static(self):
        candidates = {'recaptcha': self.config.get('RECAPTCHA_SITE_KEY', ''),
                      'turnstile': self.config.get('CLOUDFLARE_TURNSTILE_SITE_KEY', ''),
                      'fingerprint': self.config.get('FINGERPRINT_API_KEY', '')}
        try:
            result = subprocess.run(['docker', 'exec', '-i', 'superserve-devbox-console', 'node', '-e', STATIC_WRAPPER, STATIC_NODE],
                                    input=json.dumps(candidates), capture_output=True, text=True, timeout=20)
        except Exception as error:
            raise Failure(classified('static.launch', error)) from None
        if len(result.stdout) > 16384:
            raise Failure(classified('static.launch', ValueError()))
        try:
            payload = json.loads(result.stdout)
        except Exception:
            raise Failure(classified('static.launch', ValueError(), result.returncode)) from None
        if payload.get('ok') is not True or result.returncode:
            failure = payload.get('failure', {})
            stage = failure.get('stage')
            name = failure.get('exception_class')
            if stage not in STAGES or not stage.startswith('static.') or name not in CLASSES:
                raise Failure(classified('static.launch', status=result.returncode))
            number = failure.get('errno')
            raise Failure({'stage': stage, 'exception_class': name,
                           'errno': number if type(number) is int and 0 < number < 4096 else None,
                           'status': result.returncode if -128 <= result.returncode <= 255 else None})
        compiled_keys = ('recaptcha_script_code', 'turnstile_script_code', 'fingerprint_client_code',
                         'recaptcha_candidate_key_literal', 'turnstile_candidate_key_literal', 'fingerprint_candidate_key_literal')
        env_keys = ('recaptcha_api_key', 'recaptcha_project_id', 'recaptcha_site_key', 'turnstile_site_key', 'fingerprint_api_key')
        data = {}
        for group, keys in (('compiled_asset_presence', compiled_keys), ('running_server_env_presence', env_keys)):
            values = payload.get(group, {})
            if any(key not in values or (type(values[key]) is not bool and not (group == 'compiled_asset_presence' and key.endswith('_candidate_key_literal') and values[key] is None)) for key in keys):
                raise Failure(classified('static.serialize', ValueError()))
            data[group] = {key: values[key] for key in keys}
        data['absence_does_not_prove_buildtime_absence'] = True
        data['code_presence_does_not_prove_enabled_or_sdk_ready'] = True
        return data

    def observe(self):
        receipt = {'scope': 'static_only_leaf_not_carrier_clearance', 'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                   'prior_observations': None, 'runtime_before': None, 'static_presence': None, 'runtime_after': None,
                   'primary_failure': None, 'after_failure': None, 'runtime_unchanged': None,
                   'sdk_readiness': 'UNAVAILABLE_NO_BROWSER_OR_SDK_PROBE', 'root_cause': 'NOT_ESTABLISHED',
                   'counts_requeried': False, 'target_files_written': False}
        stage = 'input.prior'
        try:
            receipt['prior_observations'] = self.prior()
            stage = 'input.target'
            self.validate_target()
            stage = 'runtime.before'
            receipt['runtime_before'] = self.runtime(stage)
            stage = 'static.launch'
            receipt['static_presence'] = self.static()
        except Failure as error:
            receipt['primary_failure'] = error.evidence
        except Exception as error:
            receipt['primary_failure'] = classified(stage, error)
        finally:
            if self.target_validated:
                try:
                    receipt['runtime_after'] = self.runtime('runtime.after')
                except Failure as error:
                    receipt['after_failure'] = error.evidence
                except Exception as error:
                    receipt['after_failure'] = classified('runtime.after', error)
            if receipt['runtime_before'] is not None and receipt['runtime_after'] is not None:
                receipt['runtime_unchanged'] = receipt['runtime_before'] == receipt['runtime_after']
            receipt['status'] = 'STATIC_OBSERVATION_COMPLETE' if receipt['primary_failure'] is None and receipt['after_failure'] is None and receipt['runtime_unchanged'] is True else 'PARTIAL_OR_FAILED_STATIC_OBSERVATION'
            receipt['finished_at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        return receipt


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        raise Failure(classified('input.args', ValueError()))


def main():
    parser = SafeParser(add_help=False)
    for name in ('devbox-name', 'devbox-id', 'prior-receipt', 'prior-sha256', 'backend-sha', 'console-sha', 'binary-sha256', 'schema-sha256', 'epoch', 'compose-file', 'compose-sha256'):
        parser.add_argument('--' + name, required=True)
    try:
        args = parser.parse_args()
    except Failure as error:
        print(json.dumps({'status': 'PARTIAL_OR_FAILED_STATIC_OBSERVATION', 'primary_failure': error.evidence}))
        return 1
    receipt = Observer(args).observe()
    print(json.dumps(receipt))
    return 0 if receipt['status'] == 'STATIC_OBSERVATION_COMPLETE' else 1


if __name__ == '__main__':
    raise SystemExit(main())
