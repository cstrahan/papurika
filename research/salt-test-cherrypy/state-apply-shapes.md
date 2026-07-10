# `state.apply` behavior & return shapes

Empirical study of two things salt-api's `state.apply` does that a client must get
right:

1. **Concurrency** — what happens when a second `state.apply` lands while one is
   already running on a minion.
2. **Return shapes** — the full matrix of per-state return shapes across
   *state type × change/no-change × dry-run/real*, plus the failure shapes. This
   is reference material for building a `highstate` pretty-printer and for the
   library's normalizer.

**Version/setup:** Salt **3008.1** (`papurika-salt` container, `rest_cherrypy`,
plain HTTP :8000), single target `builtin.minion` unless noted. eauth `saltdev`
(pam).

**Reproduce:**

```bash
uv run scripts/20_concurrent_state.py   # experiment 1  -> out-state-shapes/concurrency_*.json
uv run scripts/21_state_shapes.py       # experiment 2  -> out-state-shapes/<state>_<scenario>.json
uv run scripts/22_state_full_return.py  # full_return envelope -> out-state-shapes/fullret_*.json
```

Fixtures live in `srv/salt/slow.sls` and `srv/salt/shapes/*.sls`. Captures are
verbatim request/response pairs in `out-state-shapes/`.

---

## 1. Concurrency: the per-minion state-run lock

**Salt allows only one state run per minion at a time.** A minion that is running
`state.apply` / `state.sls` / `state.highstate` holds a lock on its state
subsystem; a second state function submitted before the first finishes is
**rejected** — it does *not* queue, block, or run concurrently by default.

The rejection value is a **list containing a single string**:

```json
// concurrency_01_sync_conflict.json  (sync `local` client, HTTP 200)
{"return": [
  {"builtin.minion": [
    "The function \"state.apply\" is running as PID 64639 and was started at 2026, Jul 09 23:26:36.523925 with jid 20260709232636523925"
  ]}
]}
```

Three properties worth pinning down:

- **The lock is global to the state subsystem, not per-SLS.** While `slow` is
  running, applying an *entirely different* SLS (`demo`) is rejected with the same
  message (`concurrency_04_sync_conflict_other_sls.json`). You cannot "sneak in" a
  small unrelated state during a long run.

- **Sync vs async surface it differently.**
  - **Sync** (`local`): the rejection string comes straight back in the HTTP
    response (above).
  - **Async** (`local_async`): submission still returns a normal `jid` +
    `minions` (`concurrency_02_async_conflict_submit.json`); the rejection appears
    only when you fetch the result. In `GET /jobs/<jid>`
    (`concurrency_03_async_conflict_jobs.json`) it carries an explicit failure
    envelope:
    ```json
    "Result": {"builtin.minion": {
      "return": ["The function \"state.apply\" is running as PID ... with jid ..."],
      "retcode": 1, "success": false, "out": "highstate"}}
    ```
    So **`retcode 1` / `success false`** — a conflict is a per-minion failure, not
    a success with a funny message.

- **`queue=True` is the escape hatch — but only useful async.** Passing
  `queue=True` makes the second run *wait* for the lock instead of being rejected.
  Under the **sync** client that wait exceeds salt-api's gather timeout, so the
  HTTP call returns the "no response" string with a jid to look up later
  (`concurrency_05_sync_queue_true.json`):
  ```
  "Minion did not return. [No response]\n... salt-run jobs.lookup_jid 20260709232643841850"
  ```
  i.e. queueing a state run is a fire-and-forget-then-reconcile operation; the
  benefit is only realized by submitting async and gathering the jid. (This is the
  same "sync no-response string" surfacing documented in `cherrypy_learnings.md`.)

**Implication for papurika:** the conflict string is one of the *list-of-strings*
minion values (see §2.5). Because it comes with `retcode 1`, the normalizer
classifies it as a failed command with a message, not a highstate. The message is
worth surfacing specially in the CLI ("a state run is already in progress on
`<minion>` (jid `<jid>`)") since it's an operationally common, recoverable
condition — you usually just want to wait for or watch the running jid.

---

## 2. `state.apply` return-shape matrix

### 2.1 The envelope (`full_return: true`)

papurika **always injects `full_return: true`** at the top level of the lowstate
(`command.go` `lowstate()` — a sibling of `client`/`tgt`/`fun`, **not** a
function kwarg; in `kwarg` it is silently ignored). With it, each minion value is
wrapped:

```json
// fullret_success_nochange.json
{"builtin.minion": {
  "ret":     { /* the highstate dict, see §2.2 */ },
  "out":     "highstate",
  "retcode": 0,
  "jid":     "20260709233028352144"
}}
```

**Per-minion `retcode` taxonomy** (the cheap, reliable signal — no need to scan
every sub-state's `result`):

| `retcode` | meaning | `ret` is… |
|---|---|---|
| **0** | every state succeeded | a **dict** of states |
| **2** | states ran, **≥1 failed** (`result:false`) | a **dict** of states |
| **1** | **compile/render/lookup failure** or **concurrency conflict** — nothing ran | a **list** of error strings |

Evidence: `fullret_success_nochange` → 0; `fullret_fail_cmd` and
`fullret_test_states` (contain failing states) → 2; `fullret_render_error` and
`fullret_nonexistent_sls` → 1.

> ⚠️ **`ret` is either a dict or a list.** Any code that assumes `state.apply`
> returns `map[stateID]stateResult` will panic/misparse on the list form. Branch
> on the type. (Without `full_return`, this dict-or-list is the minion value
> directly; the trap is identical.)

### 2.2 The per-state entry (the highstate dict)

When `ret` is a dict, each key is salt's **state tag** and each value is a fixed
struct:

```json
"file_|-shapes_file_managed_|-/tmp/shapes/managed.txt_|-managed": {
  "name":        "/tmp/shapes/managed.txt",
  "result":      true,
  "comment":     "File /tmp/shapes/managed.txt updated",
  "changes":     { "diff": "New file", "mode": "0644" },
  "__id__":      "shapes_file_managed",
  "__sls__":     "shapes.file_managed",
  "__run_num__": 0,
  "start_time":  "23:24:18.549584",
  "duration":    14.373,
  "__state_ran__": true            // present & false only when SKIPPED (see §2.5)
}
```

- The **tag** is `<state>_|-<__id__>_|-<name>_|-<fun>`. Prefer the explicit
  `__id__` / `__sls__` / `name` fields over parsing the tag; take the state module
  + function from the tag's first and last `_|-` segments (e.g. `file` … `managed`).
- **`__run_num__`** is the execution order — sort by it to render states in the
  order salt ran them (map key order is not reliable).
- `duration` is **milliseconds**; `start_time` is a wall-clock time-of-day string
  (no date, no TZ).

### 2.3 `result` semantics

`result` is tri-state: **`true`** (ok), **`false`** (failed), **`null`** ("would
change" — only in `test=True`).

Dry-run (`test=True`) rules, verified:

- A state that **would change** → `result: null` (e.g. `file_managed_*_test`,
  `host_present_new_test`, `pkg_installed_would_test`).
- A state that **needs no change** → `result: true` even under test
  (`*_nochange_test`).
- A state that **fails without changing anything** → `result: false` *even under
  test* (`test.fail_without_changes` in `test_states_test`), whereas
  `test.fail_with_changes` → `result: null`. So `null` strictly means "an apply
  would have done something here."

### 2.4 `changes` is per-state-type — there is **no** common schema

The renderer cannot assume a shape for `changes`; it varies by state module and
by test-vs-real. Captured shapes (all in `out-state-shapes/`):

| state | change (real) | no-change | test / would-change |
|---|---|---|---|
| `file.managed` (new) | `{"diff":"New file","mode":"0644"}` | `{}` "in the correct state" | `{"newfile":"<path>"}` ⚠ key differs |
| `file.managed` (edit) | `{"diff":"<unified diff>"}` | — | `{"diff":"<unified diff>"}` |
| `file.directory` | `{"<path>":{"directory":"new"}}` | `{}` | `{"<path>":{"directory":"new"}}` |
| `file.absent` | `{"removed":"<path>"}` | `{}` "is not present" | `{"removed":"<path>"}` |
| `file.symlink` | `{"new":"<target>"}` | `{}` | `{"new":"<target>"}` |
| `cmd.run` | `{"pid":N,"retcode":N,"stdout":"…","stderr":"…"}` | — (runs every time) | `{"cmd":"<would-run text>"}` ⚠ key differs |
| `cmd.run` + `creates` | `{pid,retcode,stdout,stderr}` | `{}` "…exists" (`skip_watch:true`) | `{"cmd":…}` |
| `host.present` | `{"added":{"10.9.9.9":["matrix.example"]}}` | `{}` | `{"added":{…}}` |
| `user.present` | `{"gid":N,"groups":[…],"home":"","uid":N,"shell":…,…}` | `{}` | `{}` (`null` result) |
| `cron.present` | `{"<user>":"<entry>"}` | `{}` "already present" | `{}` (`null` result) |
| `pkg.installed` | `{"<pkg>":{"old":"","new":"2.10-5build1"}}` | `{}` "already installed" | `{"<pkg>":{old,new}}` |
| `module.run` | `{"<func>":<ret>}` e.g. `{"test.ping":true}` | — (runs every time) | `{}` "Function … to be executed." |
| `test.succeed_with_changes` | `{"testing":{"old":"Unchanged","new":"Something pretended to change"}}` | — | same, `result:null` |
| `service.running` | `{}` "Running in OFFLINE mode. Nothing to do" | `{}` | `{}` |

Notable asymmetries a renderer must tolerate:

- **`file.managed` uses different `changes` keys** for the same outcome:
  real-new → `{"diff":"New file","mode":…}`, but test-new → `{"newfile":"<path>"}`.
- **`cmd.run` under test** reports `{"cmd":"<text> would have been executed"}` —
  key `cmd`, not the `{pid,retcode,stdout,stderr}` of a real run.
- **`user.present` / `cron.present` show `changes:{}` under test** (with
  `result:null`) even though a real run produces rich changes — the "what will
  change" detail is not always available in dry-run.
- `service.running` returns **`result:true`, no changes, comment "Running in
  OFFLINE mode"** here because the container has no running init system — it is a
  *successful no-op*, not an error. On a real host this is where you'd see
  `{"<svc>":true}` changes on a start.

### 2.5 The list-of-strings minion value (no per-state dict at all)

Some conditions make the minion value a **list of strings** instead of the state
dict. All carry **`retcode 1`** under `full_return`. Three causes seen:

```json
// fail_render_error_real.json — Jinja/compile error
{"builtin.minion": ["Rendering SLS 'base:shapes.render_error' failed: Jinja error: division by zero\nTraceback ..."]}

// fail_nonexistent_sls_real.json — unknown SLS
{"builtin.minion": ["No matching sls found for 'shapes.nonexistent' in env 'base'"]}

// concurrency_01_sync_conflict.json — a state run already in progress (see §1)
{"builtin.minion": ["The function \"state.apply\" is running as PID ... with jid ..."]}
```

**Skipped-due-to-requisite is different** — it *stays in the dict* as a normal
per-state entry, distinguished by **`__state_ran__: false`** and a
`"One or more requisite failed: …"` comment:

```json
// fail_requisite_real.json (2nd state)
"cmd_|-depends_on_prereq_|-echo should-not-run_|-run": {
  "result": false, "changes": {},
  "comment": "One or more requisite failed: shapes.fail_requisite.prereq_fails",
  "__state_ran__": false, "__run_num__": 1, ...
}
```

So there are **three failure flavors** a renderer should distinguish:
1. **Ran and failed** — `result:false`, `changes` often populated (e.g. `cmd.run`
   `exit 3` → `changes.retcode:3`; `file.managed` bad source → `changes:{}` +
   "Unable to manage file: Source file … not found").
2. **Skipped** — `result:false`, `__state_ran__:false`, "requisite failed" comment.
3. **Never compiled** — minion value is a list of error strings; there are no
   per-state entries at all.

### 2.6 Quick reference: `result`/`changes` per captured cell

See the generated table in the captures, or `out-state-shapes/` filenames — the
scenario is encoded in the name (`<state>_<newfile|diff|nochange|new|present|gone|
run|skip|would|install>_<test|real>.json`).

---

## 3. Implications for papurika

- **Normalizer (`envelope.go` / the highstate decoder):** treat a `state.apply`
  minion value as `dict | []string`. On the list form, classify as a failed
  command (`retcode 1`) and preserve the strings as the message(s); do not attempt
  to parse states. This mirrors the existing "no per-minion dict" handling and the
  three-ways-no-response rule.
- **Per-minion status:** use the `full_return` `retcode` (0 ok / 2 states-failed /
  1 didn't-compile) as the authoritative per-minion signal; fold `2` and `1` into
  a failed `Status`. Don't infer success from HTTP 200 or from the presence of a
  dict.
- **Highstate renderer** (`renderStateMinions` / `looksHighstate` in
  `cmd/papurika/output.go`; design in `design/cli.md`; layout/color/mode ideas
  mined from salt's own formatter in
  [`highstate-formatter.md`](highstate-formatter.md))**:**
  - Sort states by `__run_num__`.
  - Derive `(module, id, name, fun)` from the tag + `__id__`/`__sls__`/`name`.
  - Map `result` → glyph: `true`→ok, `false`→fail, `null`→"would change" (test).
  - Render `changes` **generically** (recurse over whatever keys exist) with a few
    special cases for the common ones: `diff` (print the unified diff / "New
    file"), `{old,new}` pairs (pkg/testing), `{pid,retcode,stdout,stderr}`
    (cmd.run — show stdout/stderr and non-zero retcode). Never assume a schema.
  - Distinguish the three failure flavors (§2.5) in output: a skipped state
    (`__state_ran__:false`) should read differently from one that ran and failed.
  - Show a summary line (succeeded / changed / failed / would-change counts) — the
    counts come straight from `result` + non-empty `changes`.
- **`test=True` support:** the CLI's `state` command should pass `test` through;
  `result:null` is the "would change" state and must not be miscounted as success
  or failure.

---

## 4. Index of captures (`out-state-shapes/`)

**Concurrency (§1):** `concurrency_00_submit_slow_async`, `…_01_sync_conflict`,
`…_02_async_conflict_submit`, `…_03_async_conflict_jobs`,
`…_04_sync_conflict_other_sls`, `…_05_sync_queue_true`.

**full_return envelope (§2.1):** `fullret_success_nochange`, `fullret_fail_cmd`,
`fullret_test_states`, `fullret_render_error`, `fullret_nonexistent_sls`.

**Shape matrix (§2.2–2.4):** `file_managed_*`, `file_directory_*`, `file_absent_*`,
`file_symlink_*`, `cmd_creates_*`, `cmd_plain_*`, `host_present_*`,
`user_present_*`, `cron_present_*`, `pkg_installed_*`, `module_run_*`,
`service_running_*`, `test_states_*` (`_test` = dry run, `_real` = applied).

**Failures (§2.5):** `fail_source_real`, `fail_cmd_real`, `fail_requisite_real`,
`fail_render_error_real`, `fail_nonexistent_sls_real`.
