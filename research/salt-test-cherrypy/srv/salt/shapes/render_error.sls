# A Jinja render error. This does NOT produce the per-state dict shape at all —
# salt returns the minion value as a LIST of error strings. Critical shape: any
# normalizer that assumes state.apply returns dict-of-states will break here.
{{ 1 / 0 }}
placeholder:
  test.nop:
    - name: placeholder
