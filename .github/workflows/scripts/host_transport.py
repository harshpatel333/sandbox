"""How a host deploy reaches its hosts.

Fleet deploys discover instances by label and reach each one over IAP. A host
can instead deploy onto itself (DEPLOY_TRANSPORT=local, naming itself with
DEPLOY_INSTANCE and DEPLOY_ZONE) and run the identical on-host script, so a
single-box environment installs and upgrades exactly as fleet hosts do.
"""


def local_instance(env):
    """The instance a local deploy targets, or None for a fleet deploy."""
    transport = env.get("DEPLOY_TRANSPORT", "") or "gcloud"
    if transport == "gcloud":
        return None
    if transport != "local":
        raise ValueError("DEPLOY_TRANSPORT must be gcloud or local")
    name, zone = env.get("DEPLOY_INSTANCE", ""), env.get("DEPLOY_ZONE", "")
    if not name or not zone:
        raise ValueError("DEPLOY_TRANSPORT=local requires DEPLOY_INSTANCE and DEPLOY_ZONE")
    return {"name": name, "zone": zone}


def ssh_command(env, project, name, zone, script):
    if local_instance(env):
        return ["bash", "-c", script]
    return [
        "gcloud", "compute", "ssh", name,
        f"--zone={zone}", f"--project={project}",
        "--quiet", "--tunnel-through-iap", "--command", script,
    ]


def scp_command(env, project, name, zone, src, dst):
    if local_instance(env):
        return ["cp", src, dst]
    return [
        "gcloud", "compute", "scp", src, f"{name}:{dst}",
        f"--zone={zone}", f"--project={project}",
        "--quiet", "--tunnel-through-iap",
    ]
