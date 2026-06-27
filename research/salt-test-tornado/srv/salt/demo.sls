# Minimal demo state for exercising state.apply / state.sls via salt-api.
# Includes a blocking cmd.run, a managed file, a pillar-driven command, and a
# fire-and-forget background command (cmd.run_bg via module.run) so we can see
# how the two command-execution styles surface through the API.

demo_echo:
  cmd.run:
    - name: echo "hello from demo state, run at $(date -u +%FT%TZ)"

demo_managed_file:
  file.managed:
    - name: /tmp/papurika-demo.txt
    - mode: '0644'
    - contents: |
        Managed by the papurika demo state.

demo_pillar_echo:
  cmd.run:
    - name: echo "pillar greeting={{ salt['pillar.get']('demo:greeting', 'NONE') }}"

# Fire-and-forget: cmd.run_bg returns immediately (does not wait for the
# command). Driven from a state via module.run.
demo_background:
  module.run:
    - name: cmd.run_bg
    - cmd: 'sleep 15 && echo finished > /tmp/papurika-bg.txt'
