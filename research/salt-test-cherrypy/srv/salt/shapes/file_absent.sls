# file.absent — remove a path (~26 uses). Change case = path exists; no-change =
# already gone. Harness creates the file before the "change" run.
shapes_file_absent:
  file.absent:
    - name: /tmp/shapes/goner.txt
