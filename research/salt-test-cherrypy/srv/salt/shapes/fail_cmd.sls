# cmd.run that exits non-zero — result False, changes populated (retcode 3,
# stdout, stderr). The "command failed" shape.
shapes_fail_cmd:
  cmd.run:
    - name: 'echo on-stdout; echo on-stderr >&2; exit 3'
