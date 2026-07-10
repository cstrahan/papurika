# Salt's built-in `highstate` outputter — reference for papurika's renderer

We are **not** shipping salt's Python outputters, but salt's `highstate` formatter
is the reference implementation operators already know — worth mining for layout,
color, and mode ideas. This is a read of the source plus live renders of the same
`shapes/*.sls` fixtures we captured as JSON in
[`state-apply-shapes.md`](state-apply-shapes.md).

**Source:** `salt/output/highstate.py` (`output()` → `_format_host()` →
`_format_changes()` / `_format_terse()`). **Tests (golden output):**
`tests/pytests/unit/output/test_highstate.py`,
`…/test_highstate_terse.py`.

**Live renders:** `out-highstate-cli/NN_<scenario>.txt`, produced by
`scripts/23_highstate_render.sh` (`salt-call --local` against `shapes.mixed` etc.).
Reproduce:

```bash
bash scripts/23_highstate_render.sh   # -> out-highstate-cli/*.txt
```

The `mixed` fixture runs five states — a clean success, a success-with-changes, a
failure, a `file.managed` (New file), and a `cmd.run` — so one render shows every
result flavor together.

---

## 1. The full-mode block (the canonical layout)

`out-highstate-cli/01_full_verbose.txt` — one `----------`-delimited block per
state, then a summary:

```
local:
----------
          ID: mixed_changed
    Function: test.succeed_with_changes
      Result: True
     Comment: Success!
     Started: 00:18:16.164541
    Duration: 0.28 ms
     Changes:
              ----------
              testing:
                  ----------
                  new:
                      Something pretended to change
                  old:
                      Unchanged
----------
          ID: mixed_file
    Function: file.managed
        Name: /tmp/shapes/mixed.txt
      Result: True
     Comment: File /tmp/shapes/mixed.txt updated
     Started: 00:18:16.167494
    Duration: 14.337 ms
     Changes:
              ----------
              diff:
                  New file
...
Summary for local
------------
Succeeded: 4 (changed=3)
Failed:    1
------------
Total states run:     5
Total run time:  21.096 ms
```

Layout rules, exact (from `state_lines` in `_format_host`):

- Each field line is **4 spaces + an 8-char right-justified label + `: ` + value**.
  The labels are `ID`, `Name`, `Function`, `Result`, `Comment`, `Started`,
  `Duration` — e.g. `"      ID: "`, `"    Name: "`, `"  Result: "`.
- **`Name` is printed only when it differs from `ID`** (`if comps[1] != comps[2]`).
  So `test.*` states (name == id) omit it; `file.managed` (name = path) shows it.
- `Function` = `<module>.<func>` taken from the state tag's first and last
  `_|-` segments.
- `Started`/`Duration` appear only when `state_output_profile` is set (**it is, in
  this container / prod**). `Duration` gets a ` ms` suffix. `09_full_no_profile.txt`
  shows the same run without them.
- Multi-line **comments** are re-indented by 14 spaces on continuation lines.
- `Changes:` is `"     Changes:   "` followed by the changes rendered with the
  **`nested` outputter** at indent 14 (see §4).
- Block separator is exactly ten dashes (`----------`).
- The **host header** (`local:`) is the minion id, colon-terminated.

## 2. Colors (semantic, verified from `13_color.txt`)

`get_colors()` names → ANSI seen in the raw color capture:

| meaning | color | ANSI | where |
|---|---|---|---|
| success, no change | GREEN | `0;32` | state block + host header when all ok |
| changed | CYAN | `0;36` | changed state block, host header, **all summary headers** |
| failed | RED | `0;31` | failed state block; **host header if any state failed** |
| test / would-change (`result:null`) | LIGHT_YELLOW | `0;1;33` | would-change block; `unchanged=` in summary |
| compile error / warnings | LIGHT_RED | `0;1;31` | the "Data failed to compile" list (§6), warnings |
| reset | ENDC | `0;0` | end of every colored span |

Note the **host header color reflects overall status**: `hcolor` starts GREEN and
flips to RED as soon as any state has `result:false`. Handy at-a-glance signal.

## 3. Output modes

Controlled by `state_output` (`full`/`terse`/`mixed`/`changes`/`filter`) with an
`_id` modifier (`terse_id`, …) that swaps the **ID** in for the Name; plus
`state_verbose` and `state_output_diff`. Captured:

| mode | capture | behavior |
|---|---|---|
| **full** (default) | `01_full_verbose` | full block for every state |
| full, `state_verbose=False` | `02_full_no_verbose` | **hides** states that are `result:true` **and** unchanged; summary still counts them |
| **terse** | `03_terse` | one line per state: ` Name: <name> - Function: <mod>.<fun> - Result: <word>` |
| **mixed** | `04_mixed` | terse for everything **except failures**, which get the full block |
| **changes** | `05_changes` | terse only for `result:true` **and** unchanged; everything else full |
| test=True | `06_test_full` | would-change states go LIGHT_YELLOW, `Result: None`; summary gains `unchanged=N` |
| terse + tabular | `07_terse_tabular` | aligned columns w/ timestamp+duration (see below) |
| `state_output_diff=True` | `10_diff` | like verbose=False but also drops unchanged — only changed/failed shown |

**Terse result words** (`_format_terse`): `Clean` (ok, no changes), `Changed`
(has changes), `Failed` (`result:false`), `Differs` (`result:null`, i.e. test).

**Terse tabular** (`07_terse_tabular.txt`) — great for large fleets:

```
00:18:28.388341 [  1.127 ms]       test.succeed_without_changes Clean     Name: mixed_clean
00:18:28.408221 [ 85.667 ms]       file.managed    Changed   Name: /tmp/shapes/mixed.txt
00:18:28.498680 [ 16.032 ms]        cmd.run        Changed   Name: echo running the mixed command
```
format: `{start_time} [{duration:>7} ms] {module:>10}.{func:<10} {result:7}   Name: {name}`.

## 4. `Changes:` rendering — the `nested` outputter

`_format_changes` delegates to salt's **`nested`** outputter (`nested_indent=14`),
*except* when `changes` is itself a highstate (`{"out":"highstate","ret":{…}}`,
used by orchestration), which recurses into `_format_host`. So the familiar
key/`----------`/value tree is the generic nested renderer, not highstate-specific:

```
     Changes:
              ----------
              diff:
                  New file
              mode:
                  0644
```

Every scalar sits on its own line under its key; nested dicts get their own
`----------` and a deeper indent. This is verbose (a `cmd.run`'s
`{pid,retcode,stdout,stderr}` is 8+ lines) but uniform. Empty string values render
as a blank line (see `stderr:` with nothing under it in `01_full_verbose.txt`).

## 5. The summary block

```
Summary for <host>
------------
Succeeded: 4 (changed=3)
Failed:    1
------------
Total states run:     5
Total run time:  21.096 ms
```

- **`Succeeded` = count of `result` true **plus** null** (test would-change counts
  as succeeded). The parenthetical splits it: `unchanged=<null count>` (yellow,
  test only) and `changed=<nchanges>` (green).
- **`changed=N` counts states with non-empty `changes`, including *failed* ones.**
  In `11_requisite_full.txt` a failed `cmd.run` that made changes yields
  `Succeeded: 0 (changed=1)` / `Failed: 2`. So "changed" ≠ "succeeded-and-changed".
- Separator widths are computed from the label/count widths (not fixed).
- `Failed:` is RED when > 0, else CYAN.
- `state_output_pct=True` adds `Success %` / `Failure %` (`08_full_pct.txt`).
- `state_output_profile` adds `Total run time` (ms, or s when ≥ 1000 ms), summing
  per-state durations (parallel states count the max, not the sum).

## 6. Compile-error / list shape

When the minion value is a **list of strings** (render error, no-matching-sls, or a
concurrency conflict — see `state-apply-shapes.md` §2.5), highstate prints
(`12_render_error.txt`), in LIGHT_RED:

```
local:
    Data failed to compile:
----------
    Rendering SLS 'base:shapes.render_error' failed: Jinja error: division by zero
    Traceback (most recent call last):
    ...
```

No per-state blocks, no summary. `_format_host` branches on `isinstance(data,
list)` up front. (A bare string value — e.g. from `saltmod.function` — is printed
as-is, indented 4, and counted as a change.)

---

## 7. Ideas for papurika's renderer

What to **borrow**:

- **The full-mode block skeleton** (ID / Name / Function / Result / Comment /
  Changes) with 8-char aligned labels — it's clean and familiar. Keep `Name`
  suppression when it equals `ID`.
- **The color semantics** (green ok / cyan changed / red failed / yellow
  would-change) and the **host-header-reflects-worst-status** trick.
- **The mode ladder**: default full, a `terse` one-liner, and a `changes`/`diff`
  view that hides clean states. For a fleet, terse-tabular-style aligned columns
  are the right default at scale. `--state-verbose=False`/diff (hide green) is the
  single most useful knob operationally.
- **The summary line** with `changed=`/`failed=` counts and totals — but see the
  gotcha below.
- Sort by `__run_num__`; take module/func from the tag; drive Result glyph from
  `result` true/false/null.

What to **improve / change** (papurika is Go, TTY-aware, and normalizes upstream):

- **Compact the `Changes:` block.** Salt's nested renderer spends a line per scalar
  (`key:` then value indented). We can inline scalars (`key: value`) and reserve
  the multi-line tree for nested dicts — far denser for `cmd.run`/`pkg` changes.
- **Render diffs as diffs.** `changes.diff` is a unified diff (or "New file"); colorize
  `+`/`-` lines instead of dumping it under a `diff:` key. Special-case the common
  change schemas from `state-apply-shapes.md` §2.4 (`{old,new}`, cmd
  `{pid,retcode,stdout,stderr}` — surface non-zero retcode and stderr prominently).
- **Fix the "changed" count semantics** for our own summary: report
  succeeded-and-changed, failed, and (test) would-change as distinct buckets so a
  *failed*-with-changes state doesn't inflate a "changed" number that reads as
  success. (This is exactly the papurika exit-code discipline — fold all outcomes.)
- **Distinguish the three failure flavors** (ran-and-failed vs skipped-requisite
  `__state_ran__:false` vs never-compiled list) with different glyphs/labels; salt
  renders the first two identically (both RED blocks) and only special-cases the
  list.
- **Drop the profiling lines by default** on a TTY (they add two lines per state);
  offer them behind a `--timing`/verbose flag. Keep durations for the summary.
- Because we normalize in the library, our renderer consumes the unified
  `Result`/`MinionResult` model, not raw tags — but the *visual* target is this.

Design home: `cmd/papurika/output.go` (`renderStateMinions`/`looksHighstate`);
design in `design/cli.md`.

---

## 8. Index of captures (`out-highstate-cli/`)

`01_full_verbose`, `02_full_no_verbose`, `03_terse`, `04_mixed`, `05_changes`,
`06_test_full`, `07_terse_tabular`, `08_full_pct`, `09_full_no_profile`,
`10_diff`, `11_requisite_full`, `12_render_error`, `13_color` (raw ANSI).
