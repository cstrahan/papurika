#!/usr/bin/env bash
# Launch the salt-master + builtin minion test container (rest_cherrypy, plain
# HTTP on :8000 to match prod's disable_ssl: True).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker rm -f papurika-salt >/dev/null 2>&1 || true

docker run -d \
  --name papurika-salt \
  -p 8000:8000 \
  -e SALT_API_ENABLED=True \
  -e SALT_API_DISABLE_SSL=True \
  -e SALT_API_USER=saltdev \
  -e SALT_API_USER_PASS=saltdev \
  -e SALT_MINION_ENABLED=True \
  -e SALT_LOG_LEVEL=info \
  -v "${here}/config:/home/salt/data/config" \
  -v "${here}/srv:/home/salt/data/srv" \
  ghcr.io/cdalvaro/docker-salt-master:3008.1

echo "started; follow logs with: docker logs -f papurika-salt"
