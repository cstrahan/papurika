#!/usr/bin/env bash
# Launch the salt-master + builtin minion test container. salt-api serves BOTH
# netapi modules: rest_cherrypy on :8000 (image default) and rest_tornado on
# :8001 (our config/zz-papurika.conf). We map only :8001 — this harness targets
# rest_tornado. Plain HTTP (disable_ssl) to match prod.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker rm -f papurika-salt-tornado >/dev/null 2>&1 || true

docker run -d \
  --name papurika-salt-tornado \
  -p 8001:8001 \
  -e SALT_API_ENABLED=True \
  -e SALT_API_DISABLE_SSL=True \
  -e SALT_API_USER=saltdev \
  -e SALT_API_USER_PASS=saltdev \
  -e SALT_MINION_ENABLED=True \
  -e SALT_LOG_LEVEL=info \
  -v "${here}/config:/home/salt/data/config" \
  -v "${here}/srv:/home/salt/data/srv" \
  ghcr.io/cdalvaro/docker-salt-master:3008.1

echo "started; follow logs with: docker logs -f papurika-salt-tornado"
