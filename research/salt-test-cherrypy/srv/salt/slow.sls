# Long-running state used to hold the minion's state-run lock so we can observe
# what salt-api returns when a SECOND state.apply lands while one is in flight.
# See scripts/20_concurrent_state.py.
slow_sleep:
  cmd.run:
    - name: sleep 20
