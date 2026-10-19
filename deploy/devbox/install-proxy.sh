#!/bin/bash
# Build the sandbox proxy from the checked-out sandbox revision and install it
# with the fleet's legacy proxy deploy, run against this host. The same path
# serves create and upgrade: the socket holds connections across the restart,
# a failed install rolls back to the previous binary and configuration, and
# VMD is never restarted.
#
# Sandboxes are served as plain HTTP on loopback, reached through SSH
# forwarding: http://<port>-<sandbox-id>.sandbox.localhost:8090 (browsers
# resolve *.localhost to loopback), and boxd-<sandbox-id>.sandbox.localhost
# for the data plane.
set -euo pipefail

# shellcheck source=deploy/devbox/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PROXY_DOMAIN=sandbox.localhost
REDIRECT_ADDR=127.0.0.1:5008

# The fleet units listen on every interface; on the box only loopback. Kept
# as drop-ins so the fleet units install unchanged. An active socket keeps
# the addresses it bound, so a changed listener stops it for the deploy to
# rebind.
install_listeners() {
  local socket service changed=0
  socket=$(printf '[Socket]\nListenStream=\nListenStream=%s\nListenStream=%s\n' "$DEVBOX_PROXY_ADDR" "$REDIRECT_ADDR")
  service=$(printf '[Service]\nEnvironment=PROXY_ADDR=%s\nEnvironment=PROXY_REDIRECT_ADDR=%s\n' "$DEVBOX_PROXY_ADDR" "$REDIRECT_ADDR")
  install -d -m 0755 /etc/systemd/system/proxy.socket.d /etc/systemd/system/proxy.service.d
  if [ "$(cat /etc/systemd/system/proxy.socket.d/10-listen.conf 2>/dev/null)" != "$socket" ]; then
    printf '%s\n' "$socket" > /etc/systemd/system/proxy.socket.d/10-listen.conf
    changed=1
  fi
  if [ "$(cat /etc/systemd/system/proxy.service.d/10-listen.conf 2>/dev/null)" != "$service" ]; then
    printf '%s\n' "$service" > /etc/systemd/system/proxy.service.d/10-listen.conf
    changed=1
  fi
  systemctl daemon-reload
  if [ "$changed" = 1 ] && systemctl is-active --quiet proxy.socket; then
    systemctl stop proxy proxy.socket
  fi
}

main() {
  devbox_require_root
  devbox_load_env
  devbox_require_bootstrapped
  devbox_instance
  devbox_load_local_secrets SANDBOX_ACCESS_TOKEN_SEED
  systemctl is-active --quiet superserve-vmd || devbox_die "VMD is not running; run install-vmd.sh first"
  local revision
  revision=$(devbox_revision)
  devbox_log "building proxy at ${revision:0:12}"
  devbox_build "$DEVBOX_REPO/bin/proxy" ./cmd/proxy
  install_listeners
  cd "$DEVBOX_REPO"
  devbox_log "installing the proxy on $DEVBOX_INSTANCE"
  DEPLOY_TRANSPORT=local DEPLOY_INSTANCE=$DEVBOX_INSTANCE DEPLOY_ZONE=$DEVBOX_ZONE \
    GCP_REGION=$DEVBOX_REGION SHA=$revision VMD_SERVICE=superserve-vmd VMD_INSTALL_DIR=/usr/local/bin \
    PROXY_DOMAIN=$PROXY_DOMAIN PROXY_DOMAINS='' PROXY_ALLOWED_ORIGINS=$DEVBOX_CONSOLE_ORIGIN \
    REQUIRE_DATA_PLANE=1 PROXY_PEERLESS=1 PEER_ROUTING_ENABLED=0 PEER_PROXY_LISTEN_ADDR='' \
    PEER_IDENTITY_HOSTS='' PROXY_PUBLIC_SCHEME=http PROXY_HEALTH_URL="http://$DEVBOX_PROXY_ADDR/health" \
    python3 .github/workflows/scripts/deploy-proxy-legacy.py \
    || devbox_die "proxy install failed; see journalctl -u proxy"
  devbox_log "proxy ready on http://*.$PROXY_DOMAIN:${DEVBOX_PROXY_ADDR##*:}"
}

main "$@"
