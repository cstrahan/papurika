# cron.present — crontab entry (~10 uses). Needs the `crontab` binary present.
shapes_cron_present:
  cron.present:
    - name: 'echo papurika-cron'
    - user: root
    - minute: 15
    - hour: 3
