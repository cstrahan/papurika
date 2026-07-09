# file.managed whose source does not exist — a state that FAILS at runtime
# (result False) but still returns the normal per-state dict shape.
shapes_fail_source:
  file.managed:
    - name: /tmp/shapes/from_source.txt
    - source: salt://shapes/does-not-exist.txt
