# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""A matrix of state.apply per-state return shapes, for designing a pretty
printer / renderer.

Dimensions:
  (a) state type      — the common ones from our real trees, in rough frequency
                        order: file.managed, file.directory, file.absent,
                        file.symlink, cmd.run, host.present, user.present,
                        cron.present, pkg.installed, module.run, service.running,
                        plus the deterministic test.* states.
  (b) change vs none  — precondition is forced via `docker exec` (create/delete
                        the target) so we capture both the "would change / did
                        change" and the "already in the desired state" shapes.
  (c) test=True/False — dry run vs real apply.

Plus a block of FAILURE shapes (runtime failure, requisite-not-met, render
error, no-matching-sls) because a renderer has to handle those too.

Every cell is written verbatim to ../out-state-shapes/<state>_<scenario>.json.
Target is a single minion (builtin.minion) so captures aren't doubled.
"""
import os

os.environ.setdefault("PAPURIKA_OUT", "out-state-shapes")

import subprocess

import _common as c

TGT = "builtin.minion"


def sh(cmd: str):
    """Run a shell command inside the minion's container to set preconditions."""
    subprocess.run(["docker", "exec", c.CONTAINER, "sh", "-c", cmd],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def apply(name: str, sls: str, test: bool = False, timeout: int = 180):
    ls = [{"client": "local", "tgt": TGT, "fun": "state.apply",
           "arg": [sls], "kwarg": {"test": test}}]
    c.record(name, ls, c.post(token, ls, timeout=timeout))


token = c.login()

# ---------------------------------------------------------------- file.managed
# The dominant state. Three change flavors: New file, unified diff, no-change.
c.banner("file.managed")
sh("rm -rf /tmp/shapes")  # target absent -> "New file"
apply("file_managed_newfile_test", "shapes.file_managed", test=True)
apply("file_managed_newfile_real", "shapes.file_managed", test=False)
apply("file_managed_nochange_test", "shapes.file_managed", test=True)
apply("file_managed_nochange_real", "shapes.file_managed", test=False)
sh("printf 'different\\ncontents\\n' > /tmp/shapes/managed.txt")  # wrong contents -> diff
apply("file_managed_diff_test", "shapes.file_managed", test=True)
apply("file_managed_diff_real", "shapes.file_managed", test=False)

# -------------------------------------------------------------- file.directory
c.banner("file.directory")
sh("rm -rf /tmp/shapes/adir")
apply("file_directory_new_test", "shapes.file_directory", test=True)
apply("file_directory_new_real", "shapes.file_directory", test=False)
apply("file_directory_nochange_test", "shapes.file_directory", test=True)
apply("file_directory_nochange_real", "shapes.file_directory", test=False)

# ----------------------------------------------------------------- file.absent
c.banner("file.absent")
sh("mkdir -p /tmp/shapes && touch /tmp/shapes/goner.txt")  # present -> will remove
apply("file_absent_present_test", "shapes.file_absent", test=True)
apply("file_absent_present_real", "shapes.file_absent", test=False)
apply("file_absent_gone_test", "shapes.file_absent", test=True)
apply("file_absent_gone_real", "shapes.file_absent", test=False)

# ---------------------------------------------------------------- file.symlink
c.banner("file.symlink")
sh("mkdir -p /tmp/shapes && printf 'line one\\nline two\\nline three\\n' > /tmp/shapes/managed.txt && rm -f /tmp/shapes/link")
apply("file_symlink_new_test", "shapes.file_symlink", test=True)
apply("file_symlink_new_real", "shapes.file_symlink", test=False)
apply("file_symlink_nochange_test", "shapes.file_symlink", test=True)
apply("file_symlink_nochange_real", "shapes.file_symlink", test=False)

# --------------------------------------------------- cmd.run (with `creates`)
c.banner("cmd.run with creates (idempotent)")
sh("rm -f /tmp/shapes/sentinel.txt")
apply("cmd_creates_run_test", "shapes.cmd_creates", test=True)
apply("cmd_creates_run_real", "shapes.cmd_creates", test=False)
apply("cmd_creates_skip_test", "shapes.cmd_creates", test=True)
apply("cmd_creates_skip_real", "shapes.cmd_creates", test=False)

# ------------------------------------------------------- cmd.run (plain, runs)
c.banner("cmd.run plain (always runs)")
apply("cmd_plain_test", "shapes.cmd_plain", test=True)
apply("cmd_plain_real", "shapes.cmd_plain", test=False)

# --------------------------------------------------------------- host.present
c.banner("host.present")
sh("sed -i '/matrix.example/d' /etc/hosts")
apply("host_present_new_test", "shapes.host_present", test=True)
apply("host_present_new_real", "shapes.host_present", test=False)
apply("host_present_nochange_test", "shapes.host_present", test=True)
apply("host_present_nochange_real", "shapes.host_present", test=False)

# --------------------------------------------------------------- user.present
c.banner("user.present")
sh("userdel -f matrixuser 2>/dev/null; deluser matrixuser 2>/dev/null; true")
apply("user_present_new_test", "shapes.user_present", test=True)
apply("user_present_new_real", "shapes.user_present", test=False)
apply("user_present_nochange_test", "shapes.user_present", test=True)
apply("user_present_nochange_real", "shapes.user_present", test=False)

# --------------------------------------------------------------- cron.present
c.banner("cron.present")
sh("crontab -r 2>/dev/null; true")  # clear root crontab
apply("cron_present_new_test", "shapes.cron_present", test=True)
apply("cron_present_new_real", "shapes.cron_present", test=False)
apply("cron_present_nochange_test", "shapes.cron_present", test=True)
apply("cron_present_nochange_real", "shapes.cron_present", test=False)

# -------------------------------------------------------------- pkg.installed
c.banner("pkg.installed (needs apt metadata / network)")
sh("dpkg --purge hello 2>/dev/null; apt-get remove -y hello 2>/dev/null; true")
apply("pkg_installed_would_test", "shapes.pkg_installed", test=True)
apply("pkg_installed_install_real", "shapes.pkg_installed", test=False)
apply("pkg_installed_nochange_test", "shapes.pkg_installed", test=True)
apply("pkg_installed_nochange_real", "shapes.pkg_installed", test=False)

# ----------------------------------------------------------------- module.run
c.banner("module.run (not idempotent)")
apply("module_run_test", "shapes.module_run", test=True)
apply("module_run_real", "shapes.module_run", test=False)

# ------------------------------------------------------------ service.running
c.banner("service.running (systemd offline in this container)")
apply("service_running_test", "shapes.service_running", test=True)
apply("service_running_real", "shapes.service_running", test=False)

# ------------------------------------------------------------------- test.*
c.banner("test.* deterministic states (success/fail x changes/no-changes)")
apply("test_states_test", "shapes.test_states", test=True)
apply("test_states_real", "shapes.test_states", test=False)

# ============================================================ FAILURE shapes
c.banner("FAILURES: runtime failure, requisite-not-met, render error, no-such-sls")
apply("fail_source_real", "shapes.fail_source", test=False)
apply("fail_cmd_real", "shapes.fail_cmd", test=False)
apply("fail_requisite_real", "shapes.fail_requisite", test=False)
apply("fail_render_error_real", "shapes.render_error", test=False)
apply("fail_nonexistent_sls_real", "shapes.nonexistent", test=False)

c.banner("done")
