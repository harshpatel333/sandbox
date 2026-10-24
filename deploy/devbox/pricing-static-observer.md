# Static-only signup observer leaf

This is a new observer, not a retry of997 and not an approval of a carrier. RC is the sole execution owner. It preserves the completed40122720783 aggregate observations from a hash-pinned existing receipt and never requeries Auth users, fixtures or Mailpit. New target observations are fixed target guards, actual runtime identity/public state, compiled asset bytes and process-env presence. No browser, SDK, signup, provider, restart, configuration change, metadata write or privilege workaround is performed.

The leaf writes no target files: one sanitized JSON receipt is emitted to stdout. The original failure remains primary when the bounded runtime-after attempt fails too. Missing observations remain null. Static success is not an auth/browser/root-cause PASS. Missing candidate public keys yield unknown literal-presence fields; runtime env absence is not build absence and code presence is not SDK readiness.

RC must first provide and review an exact scope-correct SSH carrier. Do not use stock devbox-deploy dispatch, its configuration/metadata/checkouts, install-vmd, bootstrap or component installers. A clean leaf does not authorize stock transport or a substitute/bypass. The full envelope must use already authorized authentication, avoid SSH key/metadata provisioning, avoid target checkout/config/file writes, enforce one execution and bound the observer. Do not supply new capabilities, sudo, ptrace flags, user switching or other privilege workarounds if process-env inspection fails.

A suitable reviewed caller can verify this leaf SHA256 locally and stream exact bytes to an existing read-only SSH command with a separately reviewed hash-verifying Python stdin bootstrap. It must not upload a file or checkout onto the target. Caller implementation and the exact leaf/caller commit must be recorded before RC executes. Output/artifact handling may occur on the authorized runner under its explicit envelope; raw stderr and process/env/key values must not be logged. Do not execute a caller specified only by prose.

Required inputs are nonsecret pins/paths. Prior receipt is the already existing target JSON from40122720783; provide its exact SHA256. The compose file is an already existing file, not newly staged; its hash must match its reviewed source. The services env stays at its existing private onbox path and is consumed only by the fixed docker-compose exec invocation; no credential file is copied, created or transferred.

```sh
python3 pricing-static-observer.py \
  --devbox-name "$REVIEWED_TARGET_NAME" \
  --devbox-id "$REVIEWED_TARGET_ID" \
  --prior-receipt /var/lib/superserve-devbox/existing-attempt-readonly-ACTUAL.json \
  --prior-sha256 "$EXACT_PRIOR_FILE_SHA256" \
  --backend-sha "$EXPECTED_BACKEND_SHA" \
  --console-sha "$EXPECTED_CONSOLE_SHA" \
  --binary-sha256 "$EXPECTED_BINARY_SHA256" \
  --schema-sha256 "$EXPECTED_SCHEMA_RECEIPT_SHA256" \
  --epoch "$EXPECTED_RELEASE_EPOCH" \
  --compose-file /ALREADY_EXISTING_REVIEWED_CHECKOUT/deploy/devbox/services/compose.yml \
  --compose-sha256 "$EXACT_COMPOSE_FILE_SHA256"
```

That command describes leaf arguments, not authorization to copy a file onto the box or run from this chat. A streaming caller must pass the same arguments after validating source bytes and must have its own independent review.

Runtime-before and runtime-after each run in an isolated Python child with a20second parent deadline and at most1second reap budget; only the diagnostic worker and its dedicated process group are terminated on cleanup; subprocesses capped at10seconds, SQL statement timeout5seconds and read-only transaction. Static Node helper runs in a Node child killed by its parent after15seconds; docker exec is capped at20seconds; scan checks a10second deadline,5000files,64MiB total and8MiB individual JS limit. Existing reads may fail; do not reinterpret failures as absence. Caller should allow enough bounded time for the finally read rather than killing the observer at the first error.

Static stages: static.launch, static.stdin_parse, static.process_env, static.asset_directory, static.assets_scan, static.serialize. Error payload is only fixed stage, allowlisted class, bounded numeric errno and subprocess status. A process-env permission failure is observed as such without exposing its message or trying a privilege workaround. No raw subprocess stdout/stderr or exception text is exported on failure.

Known limitation: a failure within the single static helper stops later helper stages, preserving unknowns. This leaf diagnoses a failure boundary, not every configuration condition after that boundary. Original durable-trail absence is not specific root-cause proof. Root must decide any subsequent repair/observation under separate scope and authorization.

Local validation only:

```sh
python3 -m unittest discover -s deploy/devbox -p test_pricing_static_observer.py -v
python3 -m py_compile deploy/devbox/pricing-static-observer.py
```

Tests mock runtime commands or execute the Node helper with a synthetic in-memory filesystem. They do not connect to a target, read host process env, invoke SDKs, or access providers. They cover the original lost-error/lost-after failure mode, count preservation, unknown fields, no false completion, target guard failure and raw-output sanitization.
