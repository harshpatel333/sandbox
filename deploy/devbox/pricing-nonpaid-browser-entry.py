#!/usr/bin/env python3
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import uuid

root = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("devbox_services", root / "services/devbox_services.py")
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
config = helpers.read_env("/etc/superserve-devbox/devbox.env")
if config.get("DEVBOX_NAME") != "awille-ai-dev-billing" or config.get("DEVBOX_ID") != "29baf003ad05":
    raise SystemExit("incorrect nonpaid QA target")
backend = "4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b"
console = "587039f5e5bd9994e4e2f52a7c6a2e5daf452984"
state = Path("/var/lib/superserve-devbox")
artifacts = state / ("nonpaid-browser-" + uuid.uuid4().hex)
artifacts.mkdir(mode=0o700)
os.umask(0o077)
compose = ["docker", "compose", "--project-name", "superserve-devbox", "--file", str(root / "services/compose.yml"), "--env-file", str(state / "services.env")]

def output(args):
    return subprocess.check_output(args, text=True).strip()

def sql(statement):
    return subprocess.check_output(compose + ["exec", "-T", "db", "psql", "-X", "-q", "-At", "-U", "supabase_admin", "-d", "postgres", "-p", "54322", "-v", "ON_ERROR_STOP=1"], input="BEGIN READ ONLY; " + statement + "; ROLLBACK;", text=True).strip()

def observe():
    pid = int(output(["systemctl", "show", "superserve-devbox-api.service", "--property=MainPID", "--value"]))
    binary = Path(f"/proc/{pid}/exe")
    expected = Path(f"/opt/superserve-devbox/api/releases/{backend}/controlplane")
    if pid <= 1 or binary.resolve() != expected.resolve():
        raise RuntimeError("actual backend release changed")
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if digest != "baafe5e55d022082e0deb05c1901c9c9b78c73c6c9c904261d900265b4302567":
        raise RuntimeError("actual backend binary changed")
    inspected = json.loads(output(["docker", "inspect", "superserve-devbox-console"]))[0]
    if not inspected["State"]["Running"] or inspected["Config"]["Labels"].get("superserve-devbox.ui-revision") != console:
        raise RuntimeError("actual Console release changed")
    auth = json.loads(output(compose + ["ps", "--format", "json", "auth"]))
    auth_id = auth["ID"] if isinstance(auth, dict) else auth[0]["ID"]
    auth_env = dict(item.split("=", 1) for item in json.loads(output(["docker", "inspect", auth_id]))[0]["Config"]["Env"] if "=" in item)
    if auth_env.get("GOTRUE_SMTP_HOST") != "127.0.0.1" or auth_env.get("GOTRUE_SMTP_PORT") != "1025" or auth_env.get("GOTRUE_SITE_URL") != "http://localhost:3000" or auth_env.get("GOTRUE_MAILER_AUTOCONFIRM") != "false":
        raise RuntimeError("Auth is not the expected isolated email sink")
    sink_id = output(compose + ["ps", "-q", "mailpit"])
    sink = json.loads(output(["docker", "inspect", sink_id]))[0]
    if not sink["State"]["Running"] or any(item.startswith("MP_SMTP_RELAY") for item in sink["Config"]["Env"]) or any("smtp-relay" in item for item in (sink["Config"].get("Cmd") or [])):
        raise RuntimeError("email sink relay state is not supported")
    schema_path = state / ("pricing-e2e-" + backend) / "runtime.json"
    schema = json.loads(schema_path.read_text())
    if schema["schema_receipt"].get("sha") != backend or schema["schema_receipt"].get("migrations") not in ("applied", "none") or schema.get("binary_sha256") != digest:
        raise RuntimeError("immutable schema receipt does not match runtime")
    public = json.loads(sql("SELECT row_to_json(p) FROM billing_pricing_public_state() p"))
    adoption = json.loads(sql("SELECT json_build_object('state',state,'epoch',epoch) FROM billing_pricing_adoption WHERE singleton"))
    if public.get("pricing_version") != "v2" or public.get("entry_open") is not True or adoption.get("state") != "released" or public.get("epoch") != adoption.get("epoch"):
        raise RuntimeError("runtime is not released public v2")
    return {"backend_sha": backend, "console_sha": console, "pid": pid, "binary_sha256": digest, "console_container_id": inspected["Id"], "schema_receipt_sha256": hashlib.sha256(schema_path.read_bytes()).hexdigest(), "observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "pricing_version": public["pricing_version"], "phase": adoption["state"], "epoch": adoption["epoch"], "email_sink_only": True, "target_devbox": True}

before = observe()
(artifacts / "runtime-before.json").write_text(json.dumps(before))
limits = json.loads(sql("SELECT json_build_object('max_concurrent',max_sandboxes,'max_vcpu',max_template_vcpu,'max_memory_mib',max_template_memory_mib,'max_disk_mib',max_template_disk_mib,'max_templates',max_templates) FROM billing_plan_version WHERE family='free' AND pricing_version='v2' AND state='published'"))
(artifacts / "free-limits.json").write_text(json.dumps(limits))
pgpass = artifacts / "pgpass"
password = helpers.read_env(state / "services.env")["POSTGRES_PASSWORD"]
pgpass.write_text("127.0.0.1:54322:postgres:supabase_admin:" + password.replace("\\", "\\\\").replace(":", "\\:") + "\n")
pgpass.chmod(0o600)
argv = ["psql", "-X", "-q", "-At", "-h", "127.0.0.1", "-p", "54322", "-U", "supabase_admin", "-d", "postgres", "-v", "ON_ERROR_STOP=1"]
(artifacts / "psql-argv.json").write_text(json.dumps(argv))
command = ["docker", "run", "--rm", "--network", "host", "--shm-size", "256m", "--mount", f"type=bind,src={root / 'nonpaid-browser-qa'},dst=/qa,readonly", "--mount", f"type=bind,src={artifacts},dst=/results", "-e", "PGPASSFILE=/results/pgpass", "mcr.microsoft.com/playwright:v1.56.1-noble", "bash", "-euc", "apt-get update -qq && apt-get install -y -qq postgresql-client >/dev/null && mkdir -p /tmp/qa && cp /qa/package.json /qa/package-lock.json /qa/nonpaid-signup.mjs /tmp/qa/ && cd /tmp/qa && npm ci --ignore-scripts --no-audit --no-fund >/dev/null && node nonpaid-signup.mjs --console http://localhost:3000 --auth http://localhost:54321 --mailpit http://localhost:8025 --backend-sha " + backend + " --console-sha " + console + " --runtime-proof /results/runtime-before.json --free-limits /results/free-limits.json --psql-argv /results/psql-argv.json --output /results/browser"]
status = 1
try:
    with (artifacts / "runner-private.log").open("w") as log:
        status = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=1200).returncode
finally:
    pgpass.unlink(missing_ok=True)
    after = observe()
    (artifacts / "runtime-after.json").write_text(json.dumps(after))
    unchanged = all(before[key] == after[key] for key in ("pid", "binary_sha256", "console_container_id", "schema_receipt_sha256", "epoch", "phase", "pricing_version"))
    receipt_path = artifacts / "browser/receipt.json"
    receipt = json.loads(receipt_path.read_text()) if receipt_path.exists() else {"result": "FAIL", "failure": {"code": "browser_runner_no_receipt"}}
    result = {"scope": "dev_only_nonpaid_browser", "artifacts": str(artifacts), "runtime_before": before, "runtime_after": after, "runtime_unchanged": unchanged, "runner_status": status, "browser_receipt": receipt}
    (artifacts / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))
raise SystemExit(0 if status == 0 and unchanged and receipt.get("result") == "PASS" else 1)
