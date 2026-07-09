# module.run — invoke an execution module from a state (~25 uses). Not
# idempotent: runs every apply. Modern (kwarg-key) syntax.
shapes_module_run:
  module.run:
    - test.ping: []
