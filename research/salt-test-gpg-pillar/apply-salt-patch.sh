#!/usr/bin/env bash
# Apply the prod token/cache fix (saltstack 063aceb387) to the running
# container's bundled salt, matching the other harnesses. See the cherrypy
# harness for the full rationale. Ephemeral — re-run after recreating.
set -euo pipefail
SALT_SRC="${SALT_SRC:-$HOME/src/saltstack}"
SP=/opt/saltstack/salt/lib/python3.14/site-packages
CONTAINER="${1:-papurika-salt-gpg}"

for f in salt/auth/__init__.py salt/cache/__init__.py; do
  docker cp "${SALT_SRC}/${f}" "${CONTAINER}:${SP}/${f}"
  echo "patched ${f}"
done

docker exec "$CONTAINER" supervisorctl restart salt-master
echo "salt-master restarted"
