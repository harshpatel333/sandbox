#!/usr/bin/env python3
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.error
import urllib.request
import uuid

root = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("devbox_services", root / "services/devbox_services.py")
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
config = helpers.read_env("/etc/superserve-devbox/devbox.env")
if config.get("DEVBOX_NAME") != "awille-ai-dev-billing" or config.get("DEVBOX_ID") != "29baf003ad05":
    raise SystemExit("Incorrect integration smoke target")
state = Path("/var/lib/superserve-devbox")
env = helpers.read_env(state / "api.env")
sha = subprocess.check_output(["git", "-C", str(root.parent.parent), "rev-parse", "HEAD"], text=True).strip()
if sha != config["DEVBOX_SANDBOX_SHA"]:
    raise SystemExit("Deployed checkout does not match selected SHA")
artifacts = state / ("pricing-e2e-" + sha)
artifacts.mkdir(mode=0o700, exist_ok=True)
pid = int(subprocess.check_output(["systemctl", "show", "superserve-devbox-api.service", "--property=MainPID", "--value"], text=True))
expected_binary = Path("/opt/superserve-devbox/api/releases") / sha / "controlplane"
running_binary = Path("/proc") / str(pid) / "exe"
if pid <= 1 or running_binary.resolve() != expected_binary.resolve():
    raise SystemExit("Running controlplane is not the selected immutable release")
binary_hash = hashlib.sha256(expected_binary.read_bytes()).hexdigest()
if hashlib.sha256(running_binary.read_bytes()).hexdigest() != binary_hash:
    raise SystemExit("Running worker binary differs from the selected release")
installed = json.loads((state / "api-deploy.json").read_text())
if installed.get("sha") != sha or installed.get("migrations") not in ("applied", "none"):
    raise SystemExit("Schema installation receipt does not match the selected release")
(artifacts / "runtime.json").write_text(json.dumps({"pid": pid, "binary_sha256": binary_hash, "release": str(expected_binary), "schema_receipt": installed}, indent=2) + "\n")
compose = ["docker", "compose", "--project-name", "superserve-devbox", "--file", str(root / "services/compose.yml"), "--env-file", str(state / "services.env")]

def sql(statement):
    command = compose + ["exec", "-T", "db", "psql", "-X", "-q", "-At", "-U", "supabase_admin", "-d", "postgres", "-p", "54322", "-v", "ON_ERROR_STOP=1"]
    return subprocess.check_output(command, input=statement, text=True).strip()

def request(path, body=None, key=None, expected=(200,)):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["X-API-Key"] = key
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request("http://127.0.0.1:8080" + path, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=120) as response:
            code, payload = response.status, response.read()
    except urllib.error.HTTPError as error:
        code, payload = error.code, error.read()
    result = json.loads(payload)
    if code not in expected:
        raise RuntimeError("HTTP smoke failed " + path + " status=" + str(code) + " " + json.dumps(result))
    return result

def cli(command, *args):
    process = subprocess.run(["/opt/superserve-devbox/api/current/pricing-v2-migrate", command, "--confirm", "--actor", "dev-integration", "--reason", "authorized devbox E2E", *args], env={**os.environ, **env}, text=True, capture_output=True)
    (artifacts / (command + ".log")).write_text(process.stdout + process.stderr)
    if process.returncode:
        raise RuntimeError("Adoption " + command + " failed; inspect root-only receipt")
    return process.stdout

binding = json.loads((state / "pricing-stripe-binding.json").read_text())
if not binding.get("verified_test_mode"):
    raise SystemExit("Deployment is ready, but full dev enablement awaits verified test-mode Stripe binding")
actor = str(uuid.uuid4())
team = str(uuid.uuid4())
key = "devprobe_" + secrets.token_hex(32)
keyhash = hashlib.sha256(key.encode()).hexdigest()
sql(f"INSERT INTO profile(id,email,provider,provider_id) VALUES('{actor}','dev-probe@example.com','google','{actor}'); INSERT INTO user_role_assignments(user_id,role_id,scope_type) SELECT '{actor}',id,'platform' FROM roles WHERE name='platform_admin';")
cli("prepare")
sql(f"SELECT row_to_json(t) FROM billing_transition_pricing_version('v2','{actor}','authorized devbox E2E prepare') t;")
public = request("/billing/plans/public")
if public.get("pricing_version") != "legacy":
    raise RuntimeError("PREPARING exposed public v2")
sql(f"INSERT INTO team(id,name) VALUES('{team}','dev-worker-{team}'); INSERT INTO team_memberships(team_id,user_id,status) VALUES('{team}','{actor}','active'); INSERT INTO user_role_assignments(user_id,role_id,scope_type,team_id) SELECT '{actor}',id,'team','{team}' FROM roles WHERE name='team_owner'; INSERT INTO api_key(team_id,key_hash,name,created_by,scopes) VALUES('{team}','{keyhash}','dev-worker-probe','{actor}',ARRAY['sandbox:read','sandbox:write']); INSERT INTO team_feature_flag(team_id,key,enabled) VALUES('{team}','tenant_usage_dashboard',true) ON CONFLICT(team_id,key) DO UPDATE SET enabled=true;")
plan = request("/billing/plan", key=key)
if plan.get("plan", {}).get("kind") != "free":
    raise RuntimeError("PREPARING signup did not assign frozen Free")
request("/stripe/plan-choice", {"offer": "free"}, key=key, expected=(409,))
cli("apply", "--batch", "200")
created = request("/sandboxes", {"name": "pricing-worker-proof", "from_template": "superserve/base", "timeout_seconds": 600}, key=key, expected=(200, 201))
sandbox = str(uuid.UUID(created["id"]))
deadline = time.monotonic() + 180
while sql(f"SELECT status FROM sandbox WHERE id='{sandbox}'") != "active":
    if time.monotonic() > deadline:
        raise RuntimeError("Probe sandbox did not reach active")
    time.sleep(3)
sql(f"SELECT row_to_json(t) FROM billing_transition_pricing_version('legacy','{actor}','dev OFF committed-worker proof') t;")
off_receipt = json.loads(sql("SELECT json_build_object('pricing_version',pricing_version,'observed_at',clock_timestamp()) FROM billing_pricing_switch WHERE singleton"))
if off_receipt["pricing_version"] != "legacy":
    raise RuntimeError("Rollback was not committed before worker enqueue")
queued = int(sql(f"SELECT request_team_pause_all('{team}','plan_switch_free');"))
if queued < 1:
    raise RuntimeError("Worker probe queued no target")
deadline = time.monotonic() + 300
proof = None
while time.monotonic() < deadline:
    raw = sql(f"SELECT row_to_json(x) FROM (SELECT t.sandbox_id,t.assignment_id,t.requested_at,t.done_at,s.status,a.effective_to IS NULL AS same_open_assignment,EXISTS(SELECT 1 FROM snapshot n WHERE n.sandbox_id=t.sandbox_id AND n.created_at>=t.requested_at) AS new_snapshot,billing_pause_target_settled(t) AS settled FROM billing_team_pause_target t JOIN sandbox s ON s.id=t.sandbox_id JOIN team_plan_assignment a ON a.id=t.assignment_id WHERE t.team_id='{team}' AND t.sandbox_id='{sandbox}') x;")
    proof = json.loads(raw) if raw else None
    if proof and proof["status"] == "paused" and proof["same_open_assignment"] and proof["new_snapshot"] and proof["settled"]:
        break
    time.sleep(5)
else:
    raise RuntimeError("Deployed OFF worker did not pause/snapshot/settle the real sandbox")
if int(subprocess.check_output(["systemctl", "show", "superserve-devbox-api.service", "--property=MainPID", "--value"], text=True)) != pid:
    raise RuntimeError("API process generation changed during worker probe")
if sql("SELECT pricing_version FROM billing_pricing_switch WHERE singleton") != "legacy":
    raise RuntimeError("Worker probe did not remain OFF")
proof.update({"worker_pid": pid, "binary_sha256": binary_hash, "off_before_enqueue": off_receipt})
(artifacts / "worker.json").write_text(json.dumps(proof, indent=2) + "\n")
if request("/billing/plan", key=key).get("plan", {}).get("kind") != "free":
    raise RuntimeError("OFF lost assigned plan authority")
sql(f"SELECT row_to_json(t) FROM billing_transition_pricing_version('v2','{actor}','dev final PREPARING epoch') t;")
cli("apply", "--batch", "200")
capture = json.loads(cli("capture"))
if capture["unassigned"] or capture["deferred"] or capture["assigned"] != capture["total"]:
    raise RuntimeError("Adoption residuals block release")
start = int(subprocess.check_output(["systemctl", "show", "superserve-devbox-api.service", "--property=ActiveEnterTimestampMonotonic", "--value"], text=True)) / 1000000
while time.monotonic() - start < 360:
    time.sleep(max(0, min(5, 360 - (time.monotonic() - start))))
evidence = {"region": env["TEAM_CREATION_REGION"], "system_team_id": env["SYSTEM_TEAM_ID"], "epoch": capture["epoch"], "assembled_sha": sha, "schema_sha": sha, "worker_sha": sha, "deployment_reference": str(state / "api-deploy.json"), "worker_reference": str(artifacts / "worker.json"), "verified_worker": True}
evidence_file = artifacts / "evidence.json"
evidence_file.write_text(json.dumps(evidence))
released = json.loads(cli("release", "--epoch", capture["epoch"], "--evidence-file", str(evidence_file)))
if not released.get("released"):
    raise RuntimeError("Locked adoption release refused")
public = request("/billing/plans/public")
if public.get("pricing_version") != "v2":
    raise RuntimeError("Released public catalog is not v2")
for route in ("/auth/signup", "/plan-usage/buy-credit", "/plan-usage/upgrade", "/billing/pro", "/admin/billing/plans"):
    with urllib.request.urlopen("http://127.0.0.1:3000" + route, timeout=30) as response:
        if response.status != 200:
            raise RuntimeError("Console route unavailable " + route)
receipt = {"backend_sha": sha, "console_sha": config["DEVBOX_UI_SHA"], "test_stripe_account": binding["account_id"], "worker": proof, "released": released, "scope": "devbox HTTP/real-worker smoke; payment/calendar/browser QA remains separate"}
(artifacts / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
print(json.dumps(receipt))
