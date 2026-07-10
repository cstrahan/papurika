# A single run that exercises every result flavor at once, so one highstate
# render shows how salt lays out ok / changed / failed / (test) would-change
# side by side. Used by scripts/23_highstate_render.sh.
mixed_clean:
  test.succeed_without_changes:
    - name: mixed_clean

mixed_changed:
  test.succeed_with_changes:
    - name: mixed_changed

mixed_failed:
  test.fail_without_changes:
    - name: mixed_failed

mixed_file:
  file.managed:
    - name: /tmp/shapes/mixed.txt
    - contents: 'hello from the mixed demo'
    - makedirs: True

mixed_command:
  cmd.run:
    - name: 'echo running the mixed command'
