# Plain cmd.run — NOT idempotent: runs on every apply, always reports changes
# (stdout/stderr/retcode/pid). Contrast with cmd_creates.sls.
shapes_cmd_plain:
  cmd.run:
    - name: 'echo plain-run; echo to-stderr >&2'
