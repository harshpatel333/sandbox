# Proposed static-only SSH envelope

This is proposed source, not an available SSH identity or execution approval. Leaf253654569a847414f38b1bc3e71ddf0fb1c36f71 is pinned by SHA2561deee84a5e77170f8c5d9eeb877a867a8083e6876d6f8e68c6de99123daebb80. No existing scope-correct authenticated path has been established. Stock deployment, hostprep substitution, metadata/key provisioning and new privileges are forbidden.

An operator must supply an EXISTING private SSH configuration for alias rc-billing-readonly. Whole-envelope review must separately inspect its exact hash, all includes/proxies, remote principal, existing identity/agent and pinned known-host binding. No configuration, key or identity is provisioned here. An unreviewed Include, Match exec, proxy or credential helper is forbidden. No sudo or user switching is performed; existing permission failures are not worked around.

The configuration and nonsecret leaf-pin JSON must be regular private files with nonempty64hex reviewed SHA256 values. Validated config bytes are placed in a sealed Linux memfd held by the caller and read through its parent-PID procfs path by SSH, preventing a changed source file from changing the invoked configuration. This is runner memory, not a target configuration write; relative Includes are unsupported. The exact pin keys are devbox-name, devbox-id, prior-receipt, prior-sha256, backend-sha, console-sha, binary-sha256, schema-sha256, epoch, compose-file and compose-sha256. Paths identify already-existing target artifacts. App pins are restricted to backend4cbd9ec570e822a8de0241dc4fa2a07cd5aaa46b and Console587039f5e5bd9994e4e2f52a7c6a2e5daf452984. Prior measured zero counts are preserved without requery.

A fixed Python stdin bootstrap hashes the streamed leaf before executing it in memory. There is no target upload, checkout, receipt/configuration write, installer or service action. SSH uses BatchMode, strict known-host checking, disabled key updates/local commands/forwarding/control sockets, and one connection attempt. A proxy is acceptable only if separately reviewed and already authorized; the caller never invokes gcloud or provisions authentication.

A short configuration snapshot write fails closed before sealing or SSH. The caller creates a new0600 runner-side output and an O_EXCL one-execution marker. That marker path is fixed in the reviewed authorization; do not change it to retry. One SSH subprocess is allowed, bounded at80seconds. Possible execution is marked before dispatch. Timeout, interruption or malformed result is ambiguous and consumes the marker. There is no retry, reconnect, privilege or mutation fallback. The leaf independently bounds its own children and preserves its primary failure during finally runtime-after capture. No runtime services or engine processes are controlled.

Only whitelisted stages, classes, numeric errno/status, pinned runtime metadata, booleans/null and known prior counts reach the runner receipt. SSH stderr/raw exceptions/configuration values and extra JSON fields are discarded. Receipt collection is not successful observation: inspect actual_static_result.status and primary/after failures. Unknown is never converted to false; source/env presence does not prove SDK readiness or a signup cause.

Proposed command is NOT runnable until actual existing authentication/configuration, exact arguments and whole envelope have independent review and explicit approval:

```sh
umask 077
python3 deploy/devbox/pricing-static-observer-ssh.py \
  --ssh-config /PRIVATE/EXISTING_REVIEWED_SSH_CONFIG \
  --ssh-config-sha256 REVIEWED_CONFIG_SHA256 \
  --pins /PRIVATE/REVIEWED_NONSECRET_LEAF_PINS.json \
  --pins-sha256 REVIEWED_PINS_SHA256 \
  --once-file /PRIVATE/THIS_AUTHORIZATION_ONCE \
  --output /PRIVATE/NEW_SANITIZED_STATIC_RESULT.json
```

Local mocked tests cover the concrete failure boundaries: timeouts consume the marker, a second invocation cannot reach SSH, config drift refuses before a target call, hash-verifying transport does not stage files or change privileges, and untrusted output cannot leak or create false completion. They perform no SSH, provider, browser, database or cloud operation.
