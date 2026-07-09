# host.present — /etc/hosts entry (~34 uses). No systemd/network needed.
shapes_host_present:
  host.present:
    - name: matrix.example
    - ip: 10.9.9.9
