#!/usr/bin/env bash
# Launch a master+minion container serving BOTH netapi modules: rest_cherrypy
# on :8000 (image default) and rest_tornado on :8001 (config/zz-papurika.conf),
# so we can trigger GPG-pillar decryption via each. Plain HTTP (disable_ssl).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Free the ports from the other harness containers if present.
docker rm -f papurika-salt-gpg papurika-salt-tornado papurika-salt >/dev/null 2>&1 || true

docker run -d \
  --name papurika-salt-gpg \
  -p 8000:8000 \
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

echo "started papurika-salt-gpg (cherrypy :8000, tornado :8001)"
echo "next: bash apply-salt-patch.sh && ./gen-gpg-pillar.sh 5 && uv run scripts/stress.py"
