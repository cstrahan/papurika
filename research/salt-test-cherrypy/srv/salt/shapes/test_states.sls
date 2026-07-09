# The salt `test` state module — deterministic result/changes shapes, no side
# effects. Perfect reference cells for a pretty-printer: success/failure crossed
# with changes/no-changes, plus nop and show_notification (the latter ~6 uses).
succeed_no_changes:
  test.succeed_without_changes:
    - name: succeed_no_changes

succeed_with_changes:
  test.succeed_with_changes:
    - name: succeed_with_changes

fail_no_changes:
  test.fail_without_changes:
    - name: fail_no_changes

fail_with_changes:
  test.fail_with_changes:
    - name: fail_with_changes

a_nop:
  test.nop:
    - name: a_nop

a_notification:
  test.show_notification:
    - name: a_notification
    - text: 'this is a test.show_notification message'
