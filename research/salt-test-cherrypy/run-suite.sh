#!/usr/bin/env bash
# Run the full cherrypy probe suite in order, continuing past failures. Honors
# the _common env knobs so it can target any version:
#   SALTAPI_URL, PAPURIKA_VERIFY_SSL, PAPURIKA_OUT, SALT_CONTAINER
# Used to capture a parallel run against another salt version (e.g. 3006).
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

CONTAINER="${SALT_CONTAINER:-papurika-salt}"
SCRIPTS=(01_sync_local 02_async 03_batch 04_runners 05_wheel_keys 06_events
         07_run 08_token_longevity 09_unhappy_minion 11_long_and_suicidal_cmds
         12_jid_lifecycle 13_double_kill 14_failure_taxonomy 15_multi_command_lowstate
         16_events_robustness 17_multi_minion 18_run_eauth_token 19_minion_status_tools)

for s in "${SCRIPTS[@]}"; do
  # ensure the second minion is present before the multi-minion / status tests
  if [[ "$s" == 17_* ]]; then
    bash "$here/add-second-minion.sh" "$CONTAINER" >/dev/null 2>&1 || true
  fi
  echo "================ $s ================"
  uv run "scripts/$s.py" >/dev/null 2>&1 && echo "  ok" || echo "  (exit $?)"
done
echo "DONE: suite complete -> ${PAPURIKA_OUT:-out}/"
