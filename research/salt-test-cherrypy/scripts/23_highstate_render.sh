#!/usr/bin/env bash
# Capture what SALT ITSELF prints for a state run under the `highstate` outputter,
# across its formatting modes — reference material for papurika's pretty-printer.
# Uses `salt-call --local` inside the container against the shapes/*.sls fixtures.
#
# Output modes come from salt/output/highstate.py: state_output (full/terse/mixed/
# changes/filter), state_verbose, state_output_diff, state_output_pct,
# state_output_profile, state_tabular. Modes reachable via CLI flags use them;
# the rest are set with a config overlay that `include:`s the real minion config.
#
# Writes plain-text renders to ../out-highstate-cli/NN_<scenario>.txt (13_color is
# raw ANSI). Re-runnable; each capture resets its preconditions.
set -u
C="${SALT_CONTAINER:-papurika-salt}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$here/../out-highstate-cli"
mkdir -p "$OUT"

dex() { docker exec "$C" sh -c "$1"; }

# --- config overlays for opts with no salt-call flag -------------------------
mk_overlay() { # $1=name  $2=body (may contain \n for multiple opts)
  dex "mkdir -p /tmp/hscfg_$1 && printf 'include:\n  - /etc/salt/minion\n%b\n' '$2' > /tmp/hscfg_$1/minion"
}
mk_overlay pct        'state_output_pct: True'
mk_overlay tabular    'state_output: terse\nstate_tabular: True'
mk_overlay noprofile  'state_output_profile: False'
mk_overlay diff       'state_output_diff: True'

reset_file() { dex 'rm -f /tmp/shapes/mixed.txt' >/dev/null 2>&1; }

# cap <outfile> <salt-call-args...>   (resets the demo file first)
cap() {
  local name="$1"; shift
  reset_file
  echo "  -> $name"
  dex "salt-call --local -l quiet $*" > "$OUT/$name.txt" 2>/dev/null
}

echo "capturing highstate renders -> out-highstate-cli/"

# --- Group A: output modes (mixed.sls: ok + changed + failed together) -------
cap 01_full_verbose      --no-color state.apply shapes.mixed
cap 02_full_no_verbose   --no-color --state-verbose=False state.apply shapes.mixed
cap 03_terse             --no-color --state-output=terse state.apply shapes.mixed
cap 04_mixed             --no-color --state-output=mixed state.apply shapes.mixed
cap 05_changes           --no-color --state-output=changes state.apply shapes.mixed
cap 06_test_full         --no-color state.apply shapes.mixed test=True

# --- Group B: summary/format modifiers (config overlays) ---------------------
cap 07_terse_tabular     -c /tmp/hscfg_tabular   --no-color state.apply shapes.mixed
cap 08_full_pct          -c /tmp/hscfg_pct       --no-color state.apply shapes.mixed
cap 09_full_no_profile   -c /tmp/hscfg_noprofile --no-color state.apply shapes.mixed
cap 10_diff              -c /tmp/hscfg_diff       --no-color state.apply shapes.mixed

# --- Group C: failure / compile-error shapes ---------------------------------
cap 11_requisite_full    --no-color state.apply shapes.fail_requisite
cap 12_render_error      --no-color state.apply shapes.render_error

# --- Group D: with ANSI color (raw escape codes preserved) -------------------
cap 13_color             --force-color state.apply shapes.mixed

echo "done -> $OUT/"
