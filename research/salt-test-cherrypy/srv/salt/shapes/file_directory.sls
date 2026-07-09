# file.directory — create/enforce a directory (~54 uses).
shapes_file_directory:
  file.directory:
    - name: /tmp/shapes/adir
    - mode: '0755'
    - makedirs: True
