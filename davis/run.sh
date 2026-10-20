#!/usr/bin/env bash
# Davis (sabre/dav, CalDAV scheduling) de test, à côté de Radicale. Écoute sur 127.0.0.1:9000 ;
# exposé en HTTPS par `sudo tailscale serve --bg --https=8443 9000`. Aucun mail envoyé (INVITE_FROM_ADDRESS vide).
set -euo pipefail
cd "$(dirname "$0")"
podman rm -f davis >/dev/null 2>&1 || true
podman run -d --name davis -p 127.0.0.1:9000:9000 --env-file davis.env \
  -v "$PWD/data:/data:Z" ghcr.io/tchapi/davis-standalone:5.5.0
