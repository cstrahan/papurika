# file.managed — the single most common state in our real trees (~467 uses).
# Precondition is controlled by the harness (absent / wrong-contents / correct)
# so we can capture the "New file", unified-"diff", and no-change shapes.
shapes_file_managed:
  file.managed:
    - name: /tmp/shapes/managed.txt
    - mode: '0644'
    - makedirs: True
    - contents: |
        line one
        line two
        line three
