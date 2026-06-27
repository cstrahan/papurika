#!/usr/bin/env bash
# Add a second salt-minion (builtin.minion2) inside the master container so we
# can test real fleet behavior (aggregation, batch, partial returns). Runs a
# standalone `salt-minion -c /etc/salt/minion2 -d` pointed at the local master,
# then accepts its key via salt-key (netapi-agnostic). Idempotent.
#
#   ./add-second-minion.sh [container]
set -euo pipefail
CONTAINER="${1:-papurika-salt}"

docker exec -u root "$CONTAINER" bash -c '
set -e
PID=/var/run/salt-minion2.pid
mkdir -p /etc/salt/minion2 /etc/salt/pki/minion2 /var/cache/salt/minion2
cat > /etc/salt/minion2/minion <<EOF
id: builtin.minion2
master: localhost
master_port: 4506
pki_dir: /etc/salt/pki/minion2
cachedir: /var/cache/salt/minion2
log_file: /var/log/salt/minion2
log_level: info
pidfile: $PID
EOF
if [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; then
  echo ">>> builtin.minion2 already running (pid $(cat "$PID"))"
else
  salt-minion -c /etc/salt/minion2 -d
  echo ">>> launched builtin.minion2"
fi
'

# Give it a moment to send its key, then accept (idempotent).
sleep 5
docker exec -u root "$CONTAINER" salt-key -y -a builtin.minion2 2>&1 || true
sleep 3
echo "=== salt-key -L ==="
docker exec "$CONTAINER" salt-key -L 2>&1
