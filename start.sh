#!/usr/bin/env bash
# Starts the local test servers after a reboot: Davis (Podman) and Radicale (reference), in the background.
set -euo pipefail
cd "$(dirname "$0")"
podman start davis >/dev/null 2>&1 || ./davis/run.sh >/dev/null
if ! curl -s -o /dev/null --max-time 2 http://127.0.0.1:5232/; then
  nohup ./radicale/run.sh >> radicale/radicale.log 2>&1 &
fi
for i in $(seq 1 30); do
  curl -s -o /dev/null --max-time 1 http://127.0.0.1:9000/ && curl -s -o /dev/null --max-time 1 http://127.0.0.1:5232/ && break
  sleep 0.5
done
echo "Davis:    $(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:9000/)  http://127.0.0.1:9000/dav/"
echo "Radicale: $(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:5232/)  http://127.0.0.1:5232/"
