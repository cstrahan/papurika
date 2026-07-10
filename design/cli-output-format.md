# papurika — pretty output format

Companion to [cli.md](cli.md) §B. This specifies the **`pretty`** renderer in full:
the shared frame, the per-shape renderers, and — the reason this doc exists — the
**highstate-aware** state renderer. `pretty` is a lossy, human-facing projection of
the unified result model ([library.md](library.md) §B); `json`/`yaml`/`ndjson`
remain the machine contract and are unaffected by anything here.

Grounding: the state shapes come from
[`research/salt-test-cherrypy/state-apply-shapes.md`](../research/salt-test-cherrypy/state-apply-shapes.md);
the layout/color/mode ideas are mined from salt's own outputter in
[`research/salt-test-cherrypy/highstate-formatter.md`](../research/salt-test-cherrypy/highstate-formatter.md).
We deliberately **do not** reproduce salt's Python outputters — we render natively
from the normalized model.

Code home: `cmd/papurika/output.go` (`renderPretty` → `renderCommand` → the
per-shape renderers). This doc is the source of truth for their behavior.

---

## A. Shared frame

Every command in a result renders as:

```
<fun>[  (jid <jid>)]              ← command header (always)
  <body — one of the shapes in §C–§H>
<footer summary line>             ← one line, per command
```

The header is the salt **function** (`CommandResult.Fun`, e.g. `state.apply`); the
positional args (the sls name, the command string) live in the request, not the
result model, so they are not echoed in the header.

- **stdout carries only the render** (per cli.md §C). Banners, progress, the
  interrupt jid, and — in `--output json` — the summary go to **stderr**.
- **Indentation** is 2 spaces per level: minion lines at 2, state lines at 4,
  detail (changes/diffs) at 8.
- **Glyphs** (ASCII-safe, already used across the CLI): `✓` ok, `✗` fail, `⋯`
  no-response/pending, `●`/`○` up/down, plus (new here) `~` would-change (test)
  and `⊘` skipped-by-requisite.
- **Color** is applied only when enabled (TTY and not `--no-color`/`NO_COLOR`,
  cli.md §F). Semantic palette, borrowed from salt:

  | color | meaning |
  |---|---|
  | green | succeeded, **no** changes (clean) |
  | cyan | succeeded **with** changes |
  | red | failed |
  | yellow | would-change (test mode, `result: null`) |
  | dim | skipped, no-response, secondary text (durations, functions) |

  As in salt, the **minion header color reflects its worst state** (green all-ok,
  cyan if any changed, red if any failed) — an at-a-glance status.

Color encodes *changed-ness*; the glyph encodes *result*. So a changed-but-ok
state is a **cyan `✓`**, a failed state a **red `✗`**.

---

## B. Dispatch (which renderer)

`renderCommand` picks a body renderer from the command shape:

1. **Per-minion results present** (`local`/`local_batch`/`local_subset`/`ssh`):
   - **highstate renderer** (§D) if `useHighstate(c)` — i.e. any minion value
     *looks* like a highstate (a map whose values carry `__run_num__`) **or** the
     function is a state function (`fun` begins `state.`) and some minion value is
     a JSON array (the compile-error/list shape — §D.4). The `state.` check is what
     catches a run where *every* minion failed to compile (no dict to sniff).
   - **exec renderer** (§C) otherwise.
2. **Async submit** (a `jid`/`Targeted` but no minions yet): the fire-and-forget
   block (§F).
3. **Runner/wheel value** (no minions): `manage.*` (§G), `key.list*` (§G), else the
   generic value tree (§E).
4. **Nothing** (local target matched no minion): `(no minions matched)` (dim).

---

## C. Exec functions (`test.ping`, `cmd.run`, `grains.*`, …)

One line per minion; the return value is shown **in full** — never truncated.

```
test.ping  (jid 20260627222840835836)
  ✓ builtin.minion   True
  ⋯ builtin.minion2   (no response)
2 targeted · 1 ok · 0 failed · 1 no-response
```

- `✓` (green) ok / `✗` (red) failed (non-zero retcode or error status) / `⋯` (dim)
  no-response/pending. A non-zero retcode is noted dim after the id: `(retcode N)`.
- **Value rendering** (`renderMinionValue`), by decoded shape:
  - **map / list** → an indented tree (§E) under the id.
  - **bool** → `True` / `False` inline (salt's convention).
  - **number / null** → inline.
  - **string** → inline **iff** it is a single line ≤ 100 chars; otherwise it
    becomes an indented **block** below the id, with newlines preserved. This is
    the important case for `cmd.run`: stdout/stderr is shown whole, not clipped to
    the first line. An empty string renders `(no output)` (dim).
- Failure renders identically — same block, same fidelity — so a command's stderr
  and non-zero exit are fully visible.
- Footer: `<targeted> targeted · <ok> ok · <fail> failed · <nr> no-response`.

`run '*' cmd.run $'echo tada; echo foobar'` — multi-line output, in full:
```
cmd.run  (jid 20260710025621291825)
  ✓ web-01
      tada
      foobar
1 targeted · 1 ok · 0 failed · 0 no-response
```

`run web1 cmd.run 'exit 7'` (failed, with stderr):
```
cmd.run  (jid …)
  ✗ web1   (retcode 7)
      bash: line 1: nope: command not found
1 targeted · 0 ok · 1 failed · 0 no-response
```

> **No truncation in pretty.** Pretty shows the whole value; if that is too much,
> pipe to `--output json`/`raw` or filter downstream. (The one place we still
> abbreviate is a per-state *comment* in the highstate renderer — `firstLine` — not
> command output.) The same full-vs-block rule applies to `cmd.run` **inside a
> highstate** (§D.2 command changes render `stdout:`/`stderr:` in full).

---

## D. Highstate renderer (the state renderer)

Applies to `state.apply` / `state.sls` / `state.highstate` (and any highstate-shaped
return). The model per minion (from `state-apply-shapes.md`): `m.Value` is the
`ret` — **either** a map of per-state entries **or** a list of error strings — and
`m.Retcode` is 0 (all ok) / 2 (ran, ≥1 failed) / 1 (didn't compile).

### D.1 Layout

```
state.apply  (jid 20260627222840835836)
  ● web1        retcode 0
    ✓ nginx_pkg          pkg.installed
    ✓ nginx_conf         file.managed  /etc/nginx/nginx.conf     (New file)
        + new file
    ✓ nginx_svc          service.running                          [312ms]
  ● web2        retcode 2
    ✗ nginx_conf         file.managed   Unable to manage file: Source file salt://… not found
    ⊘ nginx_svc          service.running   requisite failed: nginx.nginx_conf
  2 minions · 4 states · 3 ok (1 changed) · 1 failed · 1 skipped
```

- **Minion header**: `● <id>   retcode <N>`, `●` colored by worst state
  (green/cyan/red). `retcode` is `m.Retcode` (the `full_return` per-minion code).
- **State line**: `<glyph> <id>   <secondary>   [duration]`.
  - **glyph + color** from `result` (+ changed-ness): `✓` green (ok, clean), `✓`
    cyan (ok, changed), `✗` red (failed), `~` yellow (`result:null`, test
    would-change), `⊘` dim (skipped — see D.3).
  - **id** is the state's `__id__` (what operators wrote); if `name` differs from
    `id` it is appended dim.
  - **secondary text**:
    - clean/changed → the state's `module.func` (dim) — tells you *what kind* of
      state at a glance (`file.managed` vs `cmd.run`).
    - failed / would-change → the **comment** (the reason: the error, or what would
      change), first line, wrapped off.
  - **duration**: appended dim as `[<n>ms]` only when notable (≥ 100 ms), so slow
    states stand out without a duration on every line. `-v` shows all durations.
  - **change detail** (§D.2) is printed indented under changed and failed states.
- **States are ordered by `__run_num__`** (salt's execution order; map order is not
  meaningful). Derived from the state tag `<module>_|-<id>_|-<name>_|-<func>`.
- **Footer** (per command, honest counts — see D.5):
  `<M> minion(s) · <S> states · <ok> ok (<changed> changed) · <failed> failed[ · <skipped> skipped][ · <wc> would-change]`.

### D.2 Change detail — compact, schema-aware

Salt renders `changes` with its generic nested outputter: a line per scalar, very
tall (a `cmd.run` is 8+ lines). We render **compactly**, special-casing the change
schemas catalogued in `state-apply-shapes.md` §2.4, falling back to a tree:

- **`diff`** (`file.managed`): if `"New file"` → `+ new file` (green). Otherwise a
  **unified diff**, one line each, colorized: `+` green, `-` red, `@@` cyan,
  context dim. Any sibling keys (e.g. `mode`) render inline after.
  ```
    ✓ nginx_conf         file.managed  /etc/nginx/nginx.conf
        --- 
        +++ 
        @@ -1,2 +1,3 @@
        -old line
        +new line one
        +new line two
        mode: 0644
  ```
- **`{old, new}` pairs** (`pkg.installed` → `{hello:{old,new}}`, `test.*` →
  `{testing:{old,new}}`, `host.present`…): `<key>: <old> → <new>` (an empty `old`
  shows as `(none) → new`).
  ```
        hello: (none) → 2.10-5build1
  ```
- **command changes** (`{pid, retcode, stdout, stderr}`): show `stdout:` (first
  line…, trimmed), `stderr:` (dim/red) when non-empty, and `exit <rc>` (red) when
  non-zero; **hide `pid`**. Empty output → nothing.
  ```
        stdout: running the mixed command
  ```
- **anything else** (`file.directory` `{path:{directory:new}}`, `module.run`
  `{fun:ret}`, `user.present` `{gid,uid,…}`): the compact tree (§E) — scalars
  inline, nested maps indented.

**Clean-removal consolidation.** `file.directory` with `clean:True` returns each
removed path **twice** — once as a per-path entry `{"<path>":{"removed":"Removed
due to clean"}}` and once in an aggregate `"removed":[<paths>]` list (verified
against salt source + 3008.1; in `test=True` mode salt emits *only* the per-path
entries). The pretty renderer folds both forms into a single `removed:` list:
```
    ✓ Remove Service Descriptors   file.directory  /etc/coronerd/services.d/
        removed:
          - /etc/coronerd/services.d/notification.json
```
This is a **presentation** fold, confined to pretty; `json`/`raw` keep salt's
verbatim (redundant) bytes, so the honesty invariant holds. It is the only such
special-case fold; other change keys pass through untouched (`file.absent`'s
scalar `{"removed":"<path>"}`, a `mode` change, etc. are left as-is).

Change detail is shown for **changed and failed** states by default. Clean states
show none. `~` would-change states show their would-change detail (the same
schema, e.g. the pending diff), since that is the whole point of a dry run.

### D.3 Failure flavors (three, distinguished)

Per `state-apply-shapes.md` §2.5, a state can be non-ok three different ways; we
render each distinctly (salt collapses the first two into identical red blocks):

1. **Ran and failed** — `result:false`, `__state_ran__` absent/true. `✗` red, the
   comment as the reason, plus change detail (e.g. `cmd.run` exit 3 → `exit 3` +
   `stderr:`).
2. **Skipped by requisite** — `result:false`, **`__state_ran__:false`**, comment
   `"One or more requisite failed: …"`. `⊘` dim; secondary text `requisite failed:
   <names>`. Counted as **skipped**, not failed, in the footer.
3. **Did not compile** — the minion value is a **list of strings** (render error /
   no-matching-sls / a concurrency conflict; §D.4). No per-state lines exist.

### D.4 Compile-error / conflict (list-valued minion)

When `m.Value` is a JSON array, there are no states — render the minion header then
the error string(s), red:

```
state.apply  (jid …)
  ● web1        retcode 1
      ✗ did not compile
        Rendering SLS 'base:broken' failed: Jinja error: division by zero
        …
  1 minion · did not compile
```

The **concurrency conflict** (`["The function \"state.apply\" is running as PID …
with jid <jid>"]`, retcode 1) is the same list shape; we detect it (the
`is running as PID … with jid` marker) and render a friendlier, actionable line
plus the recoverable jid:

```
  ● web1        retcode 1
      ✗ a state run is already in progress (jid 20260709231730801316)
        run `papurika watch 20260709231730801316` to follow it
```

### D.5 Count semantics (the fix)

Salt's summary counts `changed=N` as *any* state with changes — **including failed
ones** — so a failed-with-changes state inflates a number that reads like success
(`state-apply-shapes.md` / `highstate-formatter.md` §7). We keep the buckets
disjoint and honest, matching papurika's fold-all-outcomes exit policy (cli.md §D):

- **ok** = `result:true` (clean + changed).
- **changed** = subset of **ok** with non-empty changes. (A *failed* state's
  changes are shown inline but never counted as "changed".)
- **failed** = `result:false` that ran.
- **skipped** = `result:false` with `__state_ran__:false`.
- **would-change** = `result:null` (test).

The per-minion `retcode` remains the authoritative pass/fail signal and drives the
exit code; the state tally is the human breakdown.

### D.6 `--changes-only`

A boolean flag (persistent) that suppresses **clean** states (`result:true`, no
changes) — the single most useful knob for real highstates, which are mostly
green (salt's `state_verbose:false` / `state_output_diff`). Changed, failed,
skipped, and would-change states are always kept. A minion whose states are *all*
clean collapses to one line: `● <id>   retcode 0   (<N> states, all clean)`. The
footer counts are unaffected (they still reflect every state).

---

## E. Value tree (grains, runner/wheel returns, structured exec values)

A decoded JSON value rendered as a sorted, indented, YAML-ish tree: scalars inline
(`key: value`), maps and lists nested. Integers print without a trailing `.0`;
`null`/`true`/`false` literal. (Unchanged `writeTree`.)

```
grains.items
  ● builtin.minion
      os: Ubuntu
      osrelease: 22.04
      num_cpus: 4
      ipv4:
        - 10.0.0.5
        - 127.0.0.1
```

---

## F. Async submit (fire-and-forget)

`submit`, or any verb with `--async`: no gather, just the jid and targeted set.

```
state.highstate  (jid 20260627222843908511)
  submitted · 2 minion(s) targeted
    web1
    web2
```

---

## G. Runner/wheel special shapes

- **`manage.up`/`manage.down`** (list) and **`manage.status`** (`{up,down}`): `●`
  green per up host, `○` red per down host, footer `<u> up · <d> down`.
- **`key.list_all`**: aligned rows `accepted (N) …`, `unaccepted`, `rejected`,
  `denied`.
- Any other runner/wheel value → the value tree (§E).

(All unchanged; documented for completeness.)

---

## H. Errors, interrupts, no-match

- **Pure request failure** (auth/transport/malformed, nothing gathered): nothing on
  stdout; `papurika: <err>` on stderr; exit per the error (cli.md §D). Handled in
  `emit`, upstream of the renderer.
- **Interrupt / partial** (cli.md §E): the gathered result renders normally;
  un-returned minions show `⋯ pending`; the jid banner is on stderr.
- **No minions matched**: `(no minions matched)` (dim) + `papurika: no minions
  matched the target` on stderr; exit 2.

---

## I. Worked example — `run '*' state.apply shapes.mixed`

The `mixed` fixture (research `shapes/mixed.sls`) runs a clean success, a
success-with-changes, a failure, a `file.managed` (New file), and a `cmd.run`.
Salt's own render is 40+ lines (`out-highstate-cli/01_full_verbose.txt`); papurika:

```
state.apply  (jid 20260709...)
  ● builtin.minion        retcode 2
    ✓ mixed_clean          test.succeed_without_changes
    ✓ mixed_changed        test.succeed_with_changes
        testing: Unchanged → Something pretended to change
    ✗ mixed_failed         Failure!
    ✓ mixed_file           file.managed  /tmp/shapes/mixed.txt
        + new file
    ✓ mixed_command        cmd.run
        stdout: running the mixed command
  1 minion · 5 states · 4 ok (3 changed) · 1 failed
```
(`✓`s for the three changed states are cyan; the `✗` red; `●` red because a state
failed. `--changes-only` would drop the `mixed_clean` line.)

---

## J. Deferred (not in v1)

Salt has more modes we did *not* adopt; noted so the omission is deliberate:

- **terse / mixed / tabular** one-line-with-columns modes (`highstate-formatter.md`
  §3). Our default one-line-per-state is already terse-ish; a `--terse` column mode
  (with per-state timestamps) is a reasonable future add for large fleets.
- **`state_compress_ids`** (rolling `names`-multiplexed states into one line).
- **`state_output_pct`** success/failure percentages, and **profile totals**
  (per-run wall time). We show per-state durations for slow states instead.

If added, they belong here and behind explicit flags, not as default-behavior
changes.
