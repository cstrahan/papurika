#!/usr/bin/env bash
# Host orchestrator: (re)generate N GPG-encrypted pillar secrets in the running
# container. Creates the keydir (as root, owned by salt, 0700), then runs the
# in-container generator as the salt user.
#
#   ./gen-gpg-pillar.sh <N> [container]
set -euo pipefail

N="${1:?usage: gen-gpg-pillar.sh <N> [container]}"
CONTAINER="${2:-papurika-salt-gpg}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker exec -u root "$CONTAINER" install -d -o salt -g salt -m 700 /etc/salt/gpgkeys
docker cp "$here/_gen_secrets_incontainer.sh" "$CONTAINER:/tmp/_gen_secrets_incontainer.sh"
docker exec -u salt "$CONTAINER" bash /tmp/_gen_secrets_incontainer.sh "$N"
