#!/usr/bin/env bash
# Apply the production token/cache fix (saltstack commit 063aceb387) to the
# running container's bundled salt, to match prod and avoid the eauth-token
# eviction bug (tokens swept within ~60s despite token_expire). The container
# has no `patch` binary, and the two touched files match the v3008.1 baseline
# exactly, so we copy the fixed versions from ~/src/saltstack (HEAD = v3008.1 +
# fix) straight over the installed copies, then restart the master.
#
# NOTE: ephemeral — re-run after recreating the container.
set -euo pipefail
SALT_SRC="${SALT_SRC:-$HOME/src/saltstack}"
SP=/opt/saltstack/salt/lib/python3.14/site-packages

for f in salt/auth/__init__.py salt/cache/__init__.py; do
  docker cp "${SALT_SRC}/${f}" "papurika-salt:${SP}/${f}"
  echo "patched ${f}"
done

docker exec papurika-salt supervisorctl restart salt-master
echo "salt-master restarted"
