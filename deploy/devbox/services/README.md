# Devbox services and API

The box entry points `services-up.sh`, `install-api.sh` and `seed-base.sh`
(in `deploy/devbox/`) run the database, Auth, data API, email capture, Stripe
event forwarding and the control plane on one devbox. They run as root, read
`/etc/superserve-devbox/devbox.env` and `secrets.env`, are safe to re-run, and
accept `--dry-run` to validate configuration without changing anything.

## What runs

| Service | Image or source | Listens on |
| --- | --- | --- |
| Postgres | `supabase/postgres:17.11.0.002` | 127.0.0.1:54322 |
| Auth (GoTrue) | `supabase/gotrue:v2.197.0` | 127.0.0.1:54324, via gateway |
| Data API (PostgREST) | `postgrest/postgrest:v16.4` | 127.0.0.1:54325, via gateway |
| Gateway (`/auth/v1`, `/rest/v1`) | `caddy:2.10.2-alpine` | 127.0.0.1:54321 |
| Mailpit UI / SMTP | `axllent/mailpit:v1.31.3` | 127.0.0.1:8025 / 1025 |
| `stripe listen` | `stripe/stripe-cli:v1.31.0` | outbound only |
| API and background jobs | `cmd/controlplane` built from the checkout | 127.0.0.1:8080 |

The images match the service versions pinned by Supabase CLI 2.119.0, the CLI
the deployment migrations use. Containers share the host network and bind
loopback only; Docker runs with `iptables`, `ip6tables` and the default bridge
disabled, so it never changes the forwarding and firewall rules VMD relies on.
`services.json` records each image's digest.

The gateway requires the anon or service-role key in the `apikey` header, as
hosted Supabase does, except for CORS preflights and the Auth redirects
(`/auth/v1/verify`, `/callback`, `/authorize`). Postgres requires a password
for every TCP connection.

## Files

| Path | Contents |
| --- | --- |
| `/mnt/sandbox-data/superserve-devbox/postgres`, `mailpit` | Service data; destroying the box is the only reset |
| `/mnt/sandbox-data/superserve-devbox/local-secrets.env` | Generated on first run, never rotated (0600); linked from `/etc/superserve-devbox/local-secrets.env` |
| `/mnt/sandbox-data/superserve-devbox/promotion-account-key.pem` | Ed25519 key the console signs account assertions with (0600) |
| `/var/lib/superserve-devbox/services.json` | Images, digests and endpoints for the manifest (no secrets) |
| `/var/lib/superserve-devbox/api.env` | Rendered API environment (0600) |
| `/var/lib/superserve-devbox/api-deploy.json` | Last install: commit, migration result (`applied`, `none`, `failed`) and applied versions |
| `/opt/superserve-devbox/api/releases/<sha>` | Built `controlplane`, `seed-templates`, `hostctl`; `current` links to the running one |

`local-secrets.env` holds `POSTGRES_PASSWORD`, `DATABASE_URL` (as `postgres`), `PROMOTION_EVIDENCE_PROXY_PASSWORD`,
`JWT_SECRET`, `ANON_KEY`, `SERVICE_ROLE_KEY`, `SANDBOX_ACCESS_TOKEN_SEED`,
`INTERNAL_API_TOKEN`, `OPERATOR_API_TOKEN`, `PROMOTION_CAPTURE_TOKEN`,
`PROMOTION_ACCOUNT_TOKEN`, `PROMOTION_ACCOUNT_PUBLIC_KEY`, `SECRETS_SIGNING_KEY` (the API's sandbox JWT signing seed, published at `/internal/jwks`) and `SYSTEM_TEAM_ID`.
The console, VMD and proxy read the values they share with the API from it.

## Configuration read from CI

All optional. Without a Stripe key, billing calls and `stripe listen` stay off.

| File | Keys |
| --- | --- |
| `secrets.env` | `STRIPE_SECRET_KEY` (standard test-mode key of the devbox Stripe sandbox) |
| `devbox.env` | `DEVBOX_ID`, `DEVBOX_BUCKET` (required); `STRIPE_API_VERSION`, `STRIPE_CHECKOUT_PRICE_IDS`, `STRIPE_PRO_MONTHLY_PRICE_ID`, `STRIPE_PRO_ANNUAL_PRICE_ID`, `BILLING_RESOURCE_CONFIG` |

`host-bootstrap.sh` provides `host_id`, `host_region` and optionally
`vmd_grpc_address` in `/var/lib/superserve-devbox/runtime.json`. The API's
`DEFAULT_HOST_ID` and `TEMPLATE_BUILD_REGION` come from there.

## API configuration

Email goes to Mailpit (`EMAIL_TRANSPORT=smtp`, `SMTP_HOST=127.0.0.1`,
`SMTP_PORT=1025`), including automatic billing warnings; nothing falls back to
Resend. Template builds use `BACKUP_BUCKET=$DEVBOX_BUCKET` with the box's
service account. `stripe listen` forwards every event, plus the meter-error
thin events, to `/stripe/webhook`; `services-up.sh` passes the session's
signing secret to the API as both webhook secrets. Events for customers this
box doesn't know are acknowledged with `200 ignored`, so other boxes sharing
the Stripe sandbox are unaffected. Slack, analytics, Sentry, KMS secrets and
YC claims stay unconfigured.

## Migrations

`install-api.sh` applies `supabase/migrations` with the same pinned Supabase CLI
and `db push` history table as the deployment migrations, plus the shared-Auth
history entry (`supabase/shared-auth-history`) of the production database that
also hosts Auth, because account promotion reads it from the same database
through the RPC-only `promotion_evidence_proxy` role. Only versions missing from
`supabase_migrations.schema_migrations` run. A failing migration stops the
install before the API is restarted. A checkout that lacks an applied version
is refused; there is no rollback.

## Base host and template

`seed-base.sh` runs once at create, after VMD and the proxy. VMD registers its
host through its heartbeat; the script activates it with `hostctl` and runs
`cmd/seed-templates` for `superserve/base` only, owned by the system team that
`install-api.sh` creates. Teams that sign up on the box use that template. No
users are seeded.

## Verified only on a real box

Local runs cover the container stack, a fresh database initialization, all
migrations (fresh, re-run, one new, failing, and a refused downgrade), the API
under systemd, signup through Auth, the account-assertion keys and email
delivery to Mailpit. Template builds, host activation, ADC access to the bucket
and `stripe listen` with a real key need the box and are covered by the
end-to-end smoke run.
