#!/usr/bin/env python3
import datetime
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import urllib.parse
import urllib.request
import uuid

root = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("devbox_services", root / "services/devbox_services.py")
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
state = Path("/var/lib/superserve-devbox")
config = helpers.read_env("/etc/superserve-devbox/devbox.env")
if config.get("DEVBOX_NAME") != "awille-ai-dev-billing" or config.get("DEVBOX_ID") != "29baf003ad05":
    raise SystemExit("incorrect read-only diagnostic target")
backend = "4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b"
console = "587039f5e5bd9994e4e2f52a7c6a2e5daf452984"
fixture = "d646e99d023620f137b5"
recipient = "w0-free-" + fixture + "@example.com"
compose = ["docker", "compose", "--project-name", "superserve-devbox", "--file", str(root / "services/compose.yml"), "--env-file", str(state / "services.env")]

def run(args, body=None):
    result = subprocess.run(args, input=body, capture_output=True, text=True, timeout=90)
    if result.returncode:
        raise RuntimeError("read-only command failed; raw output withheld")
    return result.stdout.strip()

def sql(statement):
    return run(compose + ["exec", "-T", "db", "psql", "-X", "-q", "-At", "-U", "supabase_admin", "-d", "postgres", "-p", "54322", "-v", "ON_ERROR_STOP=1"], "BEGIN READ ONLY; SET LOCAL statement_timeout='10s'; " + statement + "; ROLLBACK;")

def runtime():
    pid = int(run(["systemctl", "show", "superserve-devbox-api.service", "--property=MainPID", "--value"]))
    binary = Path(f"/proc/{pid}/exe")
    if pid <= 1 or binary.resolve() != Path(f"/opt/superserve-devbox/api/releases/{backend}/controlplane"):
        raise RuntimeError("actual backend changed")
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if digest != "baafe5e55d022082e0deb05c1901c9c9b78c73c6c9c904261d900265b4302567":
        raise RuntimeError("actual binary changed")
    container = json.loads(run(["docker", "inspect", "superserve-devbox-console"]))[0]
    if not container["State"]["Running"] or container["Config"]["Labels"].get("superserve-devbox.ui-revision") != console:
        raise RuntimeError("actual Console changed")
    public = json.loads(sql("SELECT row_to_json(p) FROM billing_pricing_public_state() p"))
    return {"backend_sha": backend, "console_sha": console, "pid": pid, "binary_sha256": digest, "console_container_id": container["Id"], "pricing_version": public["pricing_version"], "entry_open": public["entry_open"], "epoch": public["epoch"]}

receipt = {"scope": "read_only_existing_attempt_only", "original_run": 40121793821, "fixture_run": fixture, "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "new_browser": False, "signup_rerun": False, "sdk_probe": False}
try:
    receipt["runtime_before"] = runtime()
    receipt["counts"] = json.loads(sql(f"""
WITH a AS (SELECT id,email_confirmed_at FROM auth.users WHERE email='{recipient}'),
p AS (SELECT id FROM profile WHERE id IN (SELECT id FROM a) OR email='{recipient}'),
t AS (SELECT DISTINCT team_id FROM team_member WHERE profile_id IN (SELECT id FROM p) AND role='owner')
SELECT json_build_object(
'auth_users',(SELECT count(*) FROM a),
'confirmed_auth_users',(SELECT count(*) FROM a WHERE email_confirmed_at IS NOT NULL),
'profiles',(SELECT count(*) FROM p),
'owner_teams',(SELECT count(*) FROM t),
'current_assignments',(SELECT count(*) FROM team_plan_assignment WHERE team_id IN (SELECT team_id FROM t) AND effective_to IS NULL AND effective_from<=CURRENT_TIMESTAMP))
"""))
    query = urllib.parse.urlencode({"query": "to:" + recipient, "limit": "1000"})
    with urllib.request.urlopen("http://127.0.0.1:8025/api/v1/search?" + query, timeout=15) as response:
        payload = json.load(response)
    messages = [message for message in payload.get("messages", []) if any(item.get("Address", "").lower() == recipient for item in message.get("To", []))]
    receipt["mailpit_counts"] = {"matching_recipient_messages": len(messages), "confirmation_subject_messages": sum(any(word in str(message.get("Subject", "")).lower() for word in ("confirm", "verify", "verification")) for message in messages), "counts_complete": int(payload.get("total", len(messages))) <= len(payload.get("messages", []))}
    candidates = {"recaptcha": config.get("RECAPTCHA_SITE_KEY", ""), "turnstile": config.get("CLOUDFLARE_TURNSTILE_SITE_KEY", ""), "fingerprint": config.get("FINGERPRINT_API_KEY", "")}
    node = r'''
const fs=require('node:fs'),path=require('node:path');
const candidate=JSON.parse(fs.readFileSync(0,'utf8'));
const env=Object.fromEntries(fs.readFileSync('/proc/1/environ','utf8').split('\0').filter(x=>x.includes('=')).map(x=>{const i=x.indexOf('=');return [x.slice(0,i),x.slice(i+1)]}));
const found={recaptcha_script_code:false,turnstile_script_code:false,fingerprint_client_code:false,recaptcha_candidate_key_literal:false,turnstile_candidate_key_literal:false,fingerprint_candidate_key_literal:false};
function scan(dir){for(const e of fs.readdirSync(dir,{withFileTypes:true})){const f=path.join(dir,e.name);if(e.isDirectory())scan(f);else if(e.isFile()&&e.name.endsWith('.js')){const data=fs.readFileSync(f,'utf8');found.recaptcha_script_code ||= data.includes('google.com/recaptcha/enterprise.js');found.turnstile_script_code ||= data.includes('challenges.cloudflare.com/turnstile/v0/api.js');found.fingerprint_client_code ||= /fingerprint|Fingerprint/.test(data);for(const key of ['recaptcha','turnstile','fingerprint'])found[key+'_candidate_key_literal'] ||= Boolean(candidate[key]&&data.includes(candidate[key]));}}}
scan('/src/apps/console/.next/static');
console.log(JSON.stringify({compiled_asset_presence:found,running_server_env_presence:{recaptcha_api_key:Boolean(env.RECAPTCHA_API_KEY),recaptcha_project_id:Boolean(env.RECAPTCHA_PROJECT_ID),recaptcha_site_key:Boolean(env.NEXT_PUBLIC_RECAPTCHA_SITE_KEY),turnstile_site_key:Boolean(env.NEXT_PUBLIC_CLOUDFLARE_TURNSTILE_SITE_KEY),fingerprint_api_key:Boolean(env.NEXT_PUBLIC_FINGERPRINT_API_KEY)},absence_does_not_prove_buildtime_absence:true,script_code_presence_does_not_prove_enabled_or_sdk_ready:true}));
'''
    receipt["static_presence"] = json.loads(run(["docker", "exec", "-i", "superserve-devbox-console", "node", "-e", node], json.dumps(candidates)))
    receipt["runtime_after"] = runtime()
    receipt["runtime_unchanged"] = receipt["runtime_before"] == receipt["runtime_after"]
    receipt["status"] = "READ_ONLY_INSPECTION_COMPLETE" if receipt["runtime_unchanged"] else "FAIL_RUNTIME_CHANGED"
    receipt["sdk_readiness"] = "UNAVAILABLE_ORIGINAL_BROWSER_CLOSED_NO_NEW_PROBE"
    receipt["root_cause"] = "NOT_ESTABLISHED"
except Exception:
    receipt["status"] = "FAIL_READ_ONLY_INSPECTION"
    receipt["failure"] = "read-only inspection failed; raw error withheld"
receipt["finished_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
output = state / ("existing-attempt-readonly-" + uuid.uuid4().hex + ".json")
output.touch(mode=0o600)
output.write_text(json.dumps(receipt, indent=2) + "\n")
print(json.dumps(receipt))
raise SystemExit(0 if receipt["status"] == "READ_ONLY_INSPECTION_COMPLETE" else 1)
