# service.running (~59 uses). systemd is NOT running in this container, so this
# is expected to ERROR — we capture that shape too (a real pretty-printer must
# render a state whose `result` is False from an internal failure, not a diff).
shapes_service_running:
  service.running:
    - name: cron
