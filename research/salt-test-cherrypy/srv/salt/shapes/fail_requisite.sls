# A failing prereq and a dependent that is therefore SKIPPED. The dependent's
# shape ("One or more requisite failed", result False, no changes, comment names
# the failed requisite) is distinct from a state that ran and failed.
prereq_fails:
  cmd.run:
    - name: 'exit 1'

depends_on_prereq:
  cmd.run:
    - name: 'echo should-not-run'
    - require:
      - cmd: prereq_fails
