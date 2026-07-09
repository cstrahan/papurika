# user.present — create a local user (~6 uses). Harness deletes the user before
# the "change" run.
shapes_user_present:
  user.present:
    - name: matrixuser
    - createhome: False
    - shell: /bin/false
