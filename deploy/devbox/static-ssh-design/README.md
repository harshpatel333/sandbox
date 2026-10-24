# Dedicated static SSH envelope design

This directory is a disabled design, not an authorized execution path. The workflow is outside `.github/workflows`, its job is false, its final step refuses execution, and `run_once.py` refuses before dispatch. Enabling any boundary requires a new exact reviewed source pin and explicit execution approval. No key generation, import, deletion, IAM mutation, metadata operation, deployment helper or target upload is implemented.

The unchanged caller is pinned to commit `69c1c752b317e2a09e344e956135dfac9841cf23`, SHA256 `1a197d5f21ffb54278fcf2cd72a1b474afb46839c5670853d0c3343c480f3e27`. The unchanged observer leaf SHA256 is `1deee84a5e77170f8c5d9eeb877a867a8083e6876d6f8e68c6de99123daebb80`. Source is sent through the caller's hash-verifying stdin bootstrap, executed in memory by the existing remote Python resolver, and never uploaded or checked out on the target.

## Fixed private review inputs

Before producing an executable workflow, independently review one immutable private envelope containing the exact instance name and numeric ID, project, zone, existing WIF provider and service-account principal, current OS Login profile ID and POSIX username, existing usable identity and public-key fingerprint, independently trusted host key or host CA and host alias, all eleven observer pins, authorization-specific once/output paths, and input hashes. No workflow input selects a target. The current deployment target must be reconciled against retained receipts; an older access command is not a target binding.

Existing GitHub environment variables provide WIF provider and service account. They prove neither present IAM access nor OS Login identity. Review current permission/profile evidence before execution; missing evidence remains a blocker. No automatic permission repair or owner/sudo substitution is permitted. The selected existing principal must already have the access required by the unmodified observer. Otherwise stop.

Render `ssh_config.template` only from that reviewed private envelope into a private runner directory. Replacement values must be single safe tokens without whitespace, quotes, percent expansion or shell metacharacters; paths must be absolute runner-owned paths. No unresolved template token, Include, Match, extra Host block, agent, forwarding, control socket or unreviewed helper is allowed. Bind the final rendered bytes by the caller's required SHA256. Review the complete effective config, SSH binary/dependencies and remote shell/Python startup behavior before any connection. Never use `ssh-keyscan`, accept-new or a discovered host key as trust establishment.

`iap_proxy.py` has only the direct `start-iap-tunnel` operation, on port22 and stdin/stdout. It never invokes `compute ssh`, which may register a key. The private0600 proxy plan contains only the reviewed absolute gcloud executable/hash and fixed instance/project/zone. The executable's Python resolver, Cloud SDK installation/dependencies, WIF credential configuration and Cloud SDK subprocess behavior remain part of the private review. A hash of the gcloud launcher alone does not authenticate its dependencies. There is no local listening port.

The future runner prepares0700 scratch with0600 config, pins, identity and known-host file. Identity import is not part of this design. The current runner skeleton intentionally never calls `invoke`. After approval, the sole allowed invocation would be the unchanged caller with exact config/pins hashes and authorization-specific once/output paths. `invoke` creates a dedicated process group with90-second outer deadline, then TERM/KILL cleanup limited to that group before scratch removal. This group must contain SSH and every proxy descendant; a helper that escapes its group is unsupported and blocks execution. Cleanup failure is ambiguous/no-retry. Runner cancellation and hard termination are not proof cleanup completed.

Retain only the caller's sanitized receipt after reviewing its contents. Do not upload config, identity, known-host data, proxy plan, Cloud SDK credentials, stderr or runner scratch. No automatic workflow retry or second once filename is allowed. A lost receipt after possible launch consumes the authorization and requires reconciliation.

## Separately proposed key capability

If no usable existing identity is available, stop. A possible future proposal is one unique run-owned public key with ten-minute OS Login expiry and exact-key-only cleanup. Approval must separately cover generation, import, existing-profile precondition, ambiguous-import reconciliation, fingerprint binding, expiry and cleanup evidence. This proposal does not implement or authorize those operations. Never remove other keys, create a POSIX profile, change metadata, grant IAM or fall back to a different principal.

## Remaining blockers

Actual current profile/principal/permissions, existing usable identity, independently trusted host key/CA, exact private config/proxy/dependency bindings, prior-receipt and compose path/hash, and authorization-specific argument/path review are not supplied by historical CI success. The PM proposal canvas was not available at the supplied workspace path; this design follows the explicit chat boundary and needs reconciliation with that document. Source review cannot clear these trust inputs or authorize dispatch.
