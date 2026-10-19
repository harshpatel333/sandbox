# Builds the console from a superserve checkout (the build context) for a
# devbox. Only public NEXT_PUBLIC_* values are build args; server secrets are
# sourced at container start from /run/superserve/console.env.
FROM oven/bun:1.3.5 AS bun

FROM node:22-bookworm-slim
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
WORKDIR /src
COPY . .
RUN bun install --frozen-lockfile

ARG NEXT_PUBLIC_APP_URL
ARG NEXT_PUBLIC_WEBSITE_URL
ARG NEXT_PUBLIC_SUPABASE_URL
ARG NEXT_PUBLIC_SUPABASE_ANON_KEY
ARG NEXT_PUBLIC_SANDBOX_SCHEME
ARG NEXT_PUBLIC_SANDBOX_HOST
ARG NEXT_PUBLIC_SANDBOX_PORT
ARG NEXT_PUBLIC_RECAPTCHA_SITE_KEY
ARG NEXT_PUBLIC_COOKIE_DOMAIN=
WORKDIR /src/apps/console
RUN bun run build && chown -R node:node .next

ARG UI_REVISION=unknown
LABEL superserve-devbox=console superserve-devbox.ui-revision=$UI_REVISION
ENV NODE_ENV=production
CMD ["bash", "-c", "set -a && . /run/superserve/console.env && set +a && exec setpriv --reuid=node --regid=node --init-groups env HOME=/home/node bun x next start -H 127.0.0.1 -p 3000"]
