# pkg.installed — package present (~122 uses). `hello` is a tiny GNU package;
# no-change case targets it once installed. Real install needs apt metadata
# (network) — the harness attempts it and records whatever comes back.
shapes_pkg_installed:
  pkg.installed:
    - name: hello
