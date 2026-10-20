#!/usr/bin/env bash
# Radicale local de test : pas d'auth (n'importe quel user/mot de passe), données dans ./cal.
set -euo pipefail
cd "$(dirname "$0")"
exec .venv/bin/python -m radicale \
  --server-hosts 0.0.0.0:5232 \
  --storage-filesystem-folder ./cal \
  --auth-type none \
  --logging-level info
