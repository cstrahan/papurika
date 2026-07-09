# file.symlink — create/enforce a symlink (~26 uses).
shapes_file_symlink:
  file.symlink:
    - name: /tmp/shapes/link
    - target: /tmp/shapes/managed.txt
    - makedirs: True
