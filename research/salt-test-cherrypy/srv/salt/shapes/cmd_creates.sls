# cmd.run with `creates` — the idempotent command shape. Runs when the sentinel
# is absent; skips ("already exists") when present. cmd.run is ~220 uses.
shapes_cmd_creates:
  cmd.run:
    - name: 'echo made > /tmp/shapes/sentinel.txt'
    - creates: /tmp/shapes/sentinel.txt
