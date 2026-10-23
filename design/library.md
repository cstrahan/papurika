# papurika — library design

Public Go API, the unified result model, the error taxonomy, the HTTP transport
(retries + re-auth), and config/secrets/token cache. See
[00-overview.md](00-overview.md) for scope and committed decisions; the async
execution engine that sits on top of this is in [async.md](async.md).

Illustrative Go throughout — signatures are the contract; bodies are sketches.
`context.Context` is the first arg on every I/O method; construction uses
functional options; errors wrap with `%w`; no global mutable state.

---

## A. Packages — public vs internal

```
github.com/cstrahan/papurika
├── client.go        PUBLIC  Client, New/NewFromConfig, Option, Login, Exec/ExecStateless
├── command.go       PUBLIC  Command (lowstate builder), JobSpec, ClientType, TgtType
├── result.go        PUBLIC  Result, CommandResult, MinionResult, Status, RawValue
├── errors.go        PUBLIC  sentinels + APIError, Module
├── run.go           PUBLIC  Run / Submit / Ping / Kill (async engine; async.md)
├── runner.go        PUBLIC  ExecAsync (async runner/wheel + return gather; async.md §B.1)
├── watch.go         PUBLIC  Watch, WatchState (async.md)
├── events.go        PUBLIC  Events() -> *EventStream (async.md)
├── jobs.go          PUBLIC  Job / Jobs, JobResult, JobMeta
├── envelope.go      (unexported) per-client-type wire decoders -> unified model
├── transport.go     (unexported) pooled *http.Client, retry, classify, re-auth
├── config/          PUBLIC  Config, Profile, Load, Resolve, SecretSource
├── state/           PUBLIC  typed decoder for state.apply returns (§B.1)
└── internal/
    ├── sse/         shared /events stream (fan-out, reconnect)  (async.md)
    └── tokencache/  0600 file store keyed by endpoint
```

**Public** is the third-party import contract — keep it small and stable: the
`Client`, the `Result` model, the error taxonomy, the `Command`/`JobSpec` builders,
`config`, and `state`.

`state` is a **separate package, and the dependency is one-way** (`state` imports
`papurika`, never the reverse). That keeps the root import contract small and
avoids a cycle, at the cost of there being no `RawValue.AsStates()` convenience
method — decoding is always the explicit `state.ParseMinion(m)`. That is the
intended ergonomics: most jobs are not state runs, and a highstate model on the
core `Result` would tax every caller for a shape only some of them ever see.

> **Implementation note — envelope/transport live in the root package, not
> `internal/`.** An earlier draft put the wire decoders and the HTTP transport in
> their own `internal/` packages. They can't be: both *produce the public types*
> (`Result`, the `APIError` sentinels), so a separate package would import
> `papurika` while `papurika` imports it — an import cycle. So `envelope.go` and
> `transport.go` are unexported code in the root package (their identifiers are
> lowercase, so they're just as hidden from third parties as `internal/` would be).
> Only the genuinely independent, cycle-free utilities — the SSE reader and the
> token-cache store — remain in `internal/`. The CLI imports only the public surface.

### The Client

```go
package papurika

type Client struct { /* unexported: transport, cfg, auth state, eventstream */ }

type Option func(*options)

func WithHTTPClient(*http.Client) Option        // caller-supplied pooled client
func WithTLSConfig(*tls.Config) Option          // per-client TLS — NOT global
func WithInsecureSkipVerify(bool) Option         // scoped to this client's tls.Config
func WithEauth(backend string) Option            // "pam" | "file" | "ldap" | ...
func WithCredentials(user string, secret config.SecretSource) Option
func WithTokenCache(s tokencache.Store) Option   // default: XDG file store
func WithRetryPolicy(RetryPolicy) Option
func WithLogger(*slog.Logger) Option             // diagnostics; defaults to no-op

func New(masterURL string, opts ...Option) (*Client, error)
func NewFromConfig(cfg *config.Config, profile string, opts ...Option) (*Client, error)
```

Method surface (detail for the async ones is in [async.md](async.md)):

```go
// Auth (usually implicit; called automatically on first use).
func (c *Client) Login(ctx context.Context) error

// Raw lowstate — to-the-metal, synchronous POST. Multi-command. (§B, this doc.)
func (c *Client) Exec(ctx context.Context, cmds ...Command) (*Result, error)        // POST /
func (c *Client) ExecStateless(ctx context.Context, cmds ...Command) (*Result, error) // POST /run

// Async engine — ergonomic, async-first. (async.md)
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (*Result, error)
func (c *Client) Submit(ctx context.Context, spec JobSpec, opts ...RunOption) (*Submission, error)
func (c *Client) Watch(ctx context.Context, jids []string, want WatchState, opts ...RunOption) (map[string]*Result, error)
func (c *Client) Ping(ctx context.Context, target, tgtType string, opts ...RunOption) (*Result, error)
func (c *Client) Kill(ctx context.Context, jid string) (*Result, error)  // saltutil.kill_job

// Jobs / events.
func (c *Client) Job(ctx context.Context, jid string) (*JobResult, error)   // GET /jobs/<jid>
func (c *Client) Jobs(ctx context.Context) (map[string]JobMeta, error)      // GET /jobs
func (c *Client) Events(ctx context.Context) (*EventStream, error)          // /events SSE
```

Two entry styles, mapping cleanly to the CLI's two tiers ([cli.md](cli.md) §A):
- **`Run`/`Submit`/`Watch`/`Ping`** — the async engine over the `local` client; the
  ergonomic, interrupt-safe path. Input is a `JobSpec`.
- **`Exec`/`ExecStateless`** — raw, synchronous `[]Command` passthrough to `POST /`
  / `POST /run`; full lowstate control, mixed client types, multi-command.

### Building commands

```go
type ClientType string
const (
    ClientLocal       ClientType = "local"
    ClientLocalAsync  ClientType = "local_async"
    ClientLocalBatch  ClientType = "local_batch"
    ClientLocalSubset ClientType = "local_subset"
    ClientRunner      ClientType = "runner"
    ClientRunnerAsync ClientType = "runner_async"
    ClientWheel       ClientType = "wheel"
    ClientWheelAsync  ClientType = "wheel_async"
    ClientSSH         ClientType = "ssh"
)

// A single lowstate dict. full_return is injected by the Client at send time
// (caller never sets it). raw map for fields we don't model.
type Command struct {
    Client  ClientType
    Fun     string
    Tgt     string                 // local/ssh
    TgtType TgtType                 // glob (default), list, pcre, grain, compound, ...
    Arg     []any
    Kwarg   map[string]any
    Batch   string                 // "10" | "25%" (local_batch)
    Extra   map[string]any         // escape hatch -> merged into the lowstate dict
}

func Local(tgt, fun string, args ...any) Command   // ClientLocal
func Runner(fun string) Command                     // ClientRunner; chain .With(kwargs)
func Wheel(fun string) Command                      // ClientWheel
func (c Command) WithKwarg(k string, v any) Command
func (c Command) WithTgtType(t TgtType) Command

// JobSpec is the async-engine convenience (one local command).
type JobSpec struct {
    Target  string
    TgtType TgtType
    Fun     string
    Arg     []any
    Kwarg   map[string]any
    Batch   string
}
```

---

## B. Unified result model (response normalization)

> salt-api returns a *different envelope per client type* (`cherrypy_learnings.md`
> "Response shapes"): `local` → `{minion: ret}`; `runner` → a bare value; `wheel` →
> `{data:{return, success}}`; async → `{jid, minions}`; `/jobs/<jid>` →
> `{info:[meta+Result], return:[{minion:ret}]}`; and `local_batch` fans results
> across one array element *per chunk*. The library collapses all of it into one
> model in `internal/envelope`, so callers see one shape.

```go
// Result is the normalized outcome of one request (which may carry N commands).
type Result struct {
    Commands []CommandResult   // parallel to the request array, in order
}

func (r *Result) JIDs() []string          // every jid this result touched
func (r *Result) Responders() []string    // minions with StatusResponded
func (r *Result) NonResponders() []string // targeted − returned (async.md §G)
func (r *Result) Failed() []MinionResult  // responded but retcode!=0, or StatusError/state-fail

// CommandResult is one lowstate command's outcome.
type CommandResult struct {
    Client   ClientType
    Fun      string

    // async submit (local_async / POST /minions): the jid + the AUTHORITATIVE
    // targeted set (salt's "minions" list). Minions fills in as returns arrive.
    JID      string
    Targeted []string

    // local / local_batch: per-minion outcomes.
    Minions  map[string]MinionResult

    // runner / wheel: single value, no per-minion fanout.
    Value    RawValue
    Retcode  *int    // nil = salt reported none
    Success  *bool   // wheel/runner full_return success

    Complete bool             // async.md: all targeted terminal vs deadline/cancel
    Reason   CompletionReason

    raw json.RawMessage       // verbatim return[i] (honesty)
}

type MinionResult struct {
    ID      string
    Status  Status
    Retcode *int        // from full_return; nil if unavailable
    Value   RawValue    // the function's return value, unwrapped
    raw     json.RawMessage
}

type Status int
const (
    StatusPending    Status = iota // targeted, not yet returned (zero value)
    StatusResponded                // returned a value
    StatusNoResponse               // targeted but never returned
    StatusError                    // structured per-minion error (e.g. "'x' is not available.")
    StatusNotMatched               // target matched no minion
)
```

**Typed access without losing honesty.** `RawValue` wraps the verbatim
`json.RawMessage`; callers decode at the type they expect, or reach the raw bytes:

```go
type RawValue struct{ raw json.RawMessage }

func (v RawValue) String() (string, bool)
func (v RawValue) Bool() (bool, bool)
func (v RawValue) Int() (int64, bool)
func (v RawValue) Map() (map[string]any, bool)
func (v RawValue) Decode(into any) error   // json.Unmarshal into a caller struct
func (v RawValue) Raw() json.RawMessage    // exact wire bytes — escape hatch
```

**`full_return: true` is always sent** (injected by the `Client` into every
non-wheel command; wheel is already wrapped). It gives a uniform per-minion
`{ret, retcode, jid}` and `{data:{return, success}}` for runner/wheel, and surfaces
`retcode` — exactly pepper's one good normalization, kept.

### Wire-shape → model mapping (`internal/envelope`)

| Observed wire shape | Normalization |
|---|---|
| `local`+full_return `{m:{ret,retcode,jid}}` | `MinionResult{Responded, Retcode:&rc, Value:ret}` per key |
| `local` `"Minion did not return. [No response]…"` | `StatusNoResponse` (raw string kept) |
| `local` `"'x' is not available."` | `StatusError` (message in raw) |
| `local` fast empty `[{}]` | command-level: targeted⇒`StatusNotMatched`/empty |
| `local_batch` `[{chunk1},{chunk2}…]` | **merge** per-chunk maps → one `Minions`; unwrap splice (use `ret`, else dict−`retcode`). Lossless: ids are unique keys (`pepper_design_analysis.md` §2.12) |
| `local_async` / `POST /minions` `{jid,minions}` | `JID` + `Targeted`; `Minions` empty until gathered |
| `runner` bare / full_return `{data:{return,success}}` | `Value` ← `data.return` (or bare); `Success` ← `data.success` |
| `wheel` `{tag,data:{return,success}}` | `Value` ← `data.return`; `Success` ← `data.success` |
| `GET /jobs/<jid>` `{info:[meta+Result],return:[…]}` | per-minion from `info[0].Result`; `Targeted` ← `info[0].Minions`; **non-responders = Targeted − Result keys** ⇒ `StatusNoResponse`; `info[0].Error` ⇒ `ErrJobNotFound` |

The three ways "no response" surfaces (sync string / async omission / missing `ret`
event) all fold into `StatusNoResponse` — callers never pattern-match English.

### B.1 State returns (`state/`)

A `state.apply` return is the one minion value the core model deliberately does
**not** fold in: it is a map of per-state results whose `changes` payload has no
common schema (it differs per state module *and* per test-vs-real run — see
`state-apply-shapes.md`). Putting that on `MinionResult` would tax every caller
for a shape only some of them ever see. So `MinionResult.Value` keeps the raw
bytes and package `state` decodes them on request:

```go
sr, err := state.ParseMinion(m)      // or state.Parse(m.Value) without a retcode
```

This is still **normalization in the library, not the formatter** — the rule the
CLI's renderer previously broke by keeping this model unexported in
`cmd/papurika/output.go`. The renderer now consumes `state` like any other
caller.

What it folds:

| Observed wire shape | Decoded as |
|---|---|
| map of `<module>_|-<id>_|-<name>_|-<fun>` → entry | `Result.Entries`, sorted by `__run_num__` (salt's execution order) |
| `result: true` / `false` / `null` | `Changed`/`Clean` · `Failed` · `WouldChange` — three outcomes, not a bool |
| `result:false` + `__state_ran__:false` | `Skipped` (a failed requisite, *not* a failed state) |
| **list** of strings + `"is running as PID … with jid <jid>"` | `*ConflictError{JID}` |
| **list** of strings otherwise (render error, missing SLS) | `*CompileError{Messages}` |

The list form is the trap: a run that was refused or never compiled returns a
JSON array, not a map. Decoding it into an empty map — which is what a naive
`map[string]entry` unmarshal does — turns a refused run into a clean one. It is
reported as a typed error, never an empty `Result`.

`Result.OK()` prefers the minion's **retcode** (`0` = all ok, `2` = ran with
failures; `1` = never ran, which surfaces as one of the errors above). That is
salt's own verdict and is cheaper and more trustworthy than re-deriving one by
scanning sub-states.

---

## C. Error taxonomy (idiomatic Go errors)

The discipline: **request-level failures are Go `error`s; per-minion/per-command
outcomes are not** — they're a `Status`/`Retcode` on the result (§B). An
unknown function, a non-zero retcode, a failed state, a non-responder: those are
*results*, inspected on the `Result`. Only a whole-call failure (auth, malformed
request, transport) returns a non-nil `error` from `Exec`/`Run`.

```go
var (
    ErrAuthFailure = errors.New("papurika: authentication failed")  // bad creds / token (authn)
    ErrAuthDenied  = errors.New("papurika: authorization denied")   // valid token, missing perm (authz)
    ErrNotFound    = errors.New("papurika: not found")
    ErrJobNotFound = errors.New("papurika: job not found")          // info[0].Error on /jobs/<jid>
    ErrMalformed   = errors.New("papurika: malformed request")      // 400 bad JSON; failed client-side lowstate validation
    ErrServerError = errors.New("papurika: server error")           // 500 — the server failed, our request was well-formed
    ErrNoResponse  = errors.New("papurika: minion did not respond")  // only when caller opts into strict
    ErrTransport   = errors.New("papurika: transport error")        // network / TLS / ctx deadline
)

// APIError carries HTTP context and wraps one sentinel (errors.Is/As both work).
type APIError struct {
    Kind        error   // one of the sentinels
    StatusCode  int
    ContentType string
    Body        string  // raw (may be HTML) — diagnostics only, never trusted as a result
    Module      Module  // best-effort: cherrypy | tornado
}
func (e *APIError) Error() string { /* ... */ }
func (e *APIError) Unwrap() error { return e.Kind }

type Module int
const (ModuleUnknown Module = iota; ModuleCherryPy; ModuleTornado)
```

**Classifier (in `internal/transport`)** — inspects status *and* body, and never
assumes JSON on non-2xx (`cherrypy_learnings.md` failure taxonomy):

| Observed | Classified |
|---|---|
| 401 + HTML `"Authorization error occurred."` | `ErrAuthDenied` (cherrypy authz) |
| 401 + "Please log in" / wrong password | `ErrAuthFailure` (authn → triggers re-auth, §D) |
| **200** + `return[]=="Failed to authenticate"` | `ErrAuthDenied` (tornado authz — a *200*) |
| 400 (HTML or JSON) | `ErrMalformed` (bad JSON body) |
| 500 `"An unexpected error occurred"` | `ErrServerError` — the missing-fun/tgt cause is excluded by client-side validation before sending |
| **200** + `return[]=="Unexpected exception while handling request…"` | `ErrMalformed` (tornado missing fun/tgt) |
| `info[0].Error` on `/jobs/<jid>` | `ErrJobNotFound` |
| network / TLS / `ctx` deadline | `ErrTransport` (wraps `context.DeadlineExceeded` etc.) |
| 202 on `POST /minions` | success (not an error) |

Caller usage:

```go
res, err := c.Run(ctx, spec)
switch {
case errors.Is(err, papurika.ErrAuthDenied): // fix perms/eauth — re-login won't help
case errors.Is(err, papurika.ErrMalformed):  // our request is wrong
case errors.Is(err, papurika.ErrServerError): // the master failed, not us
case errors.Is(err, papurika.ErrTransport):  // network — maybe retry / inspect
case err != nil:
    var ae *papurika.APIError
    if errors.As(err, &ae) { log.Printf("HTTP %d: %s", ae.StatusCode, ae.Body) }
}
// Per-minion outcomes are NOT errors:
for _, m := range res.Commands[0].Minions {
    switch m.Status {
    case papurika.StatusResponded:
        if m.Retcode != nil && *m.Retcode != 0 { /* command failed on this minion */ }
    case papurika.StatusNoResponse: /* down/unreachable */
    case papurika.StatusError:      /* e.g. unknown function */
    }
}
```

This split is what lets the CLI's exit-code policy be a pure function over the
`Result` across **all** minions and commands ([cli.md](cli.md) §D) — fixing pepper's
"only inspect `result[0]`" bug.

---

## D. Transport — retries & re-auth (`internal/transport`)

Sits below the public `Client`. Owns one pooled `*http.Client`, the retry loop, the
classifier (§C), and a single-flight re-auth callback.

```go
type Transport struct {
    httpc  *http.Client
    retry  RetryPolicy
    reauth func(ctx context.Context) (token string, err error) // re-run /login (or re-mint eauth token)
    mu     sync.Mutex                                           // single-flight re-auth
}

func newHTTPClient(tlsCfg *tls.Config, timeout time.Duration) *http.Client {
    return &http.Client{
        Timeout: timeout, // a generous transport-level safety net; ctx is the real bound (async.md §B)
        Transport: &http.Transport{
            TLSClientConfig:     tlsCfg, // per-client, NOT a global default — fixes pepper §2.6
            MaxIdleConns:        64,
            MaxIdleConnsPerHost: 16,
            IdleConnTimeout:     90 * time.Second,
            ForceAttemptHTTP2:   true,
        },
    }
}

type RetryPolicy struct {
    MaxAttempts int           // default 3
    BaseBackoff time.Duration // default 200ms, exponential + full jitter
    MaxBackoff  time.Duration // default 5s
}
```

**Connection reuse:** one pooled client for the Client's lifetime (pepper opened a
fresh connection per request). **TLS is per-client** via `WithTLSConfig` /
`WithInsecureSkipVerify` — never the global process default.

**Retry only what's safe.** The axis is not "is this request idempotent" but
**"did this request provably never execute"** — salt has no idempotency key, so a
submit that landed and then failed on the way back leaves a job running, and
re-sending starts a *second* execution under a second jid that nobody is
watching.

- **Idempotent by construction** — GETs (`/jobs`, `/jobs/<jid>`, `/`) and
  `POST /login` (mints a fresh token): retried on any transient failure
  (`429`, `502`, `503`, `504`, network errors).
- **Command POSTs** (`/`, `/run`) are job submissions. They are retried **only**
  on failures that prove salt never acted:
  - connection-establishment failures — DNS, `ECONNREFUSED`, host/net
    unreachable. Nothing was written, and this is the master-is-restarting case.
  - **`429`** — and only because *salt-api never emits one* (there is no `429`
    anywhere in `salt/netapi`), so it can only have come from an intermediary
    that declined to forward the request.
- **Everything else on a command POST is ambiguous** and is sent exactly once:
  - `500` — the master may have crashed after publishing.
  - `502` — a proxy failure says nothing about the upstream.
  - **`503` — ambiguous, which is not obvious.** salt-api raises it for
    `SaltDaemonNotRunning` *and* `SaltReqTimeoutError`
    (`rest_cherrypy/app.py`). The first is safe; the second means a master
    request call didn't answer in time, which says nothing about whether the
    master received it and published anyway. Both arrive as a bare `503`, so a
    congested master is indistinguishable from an absent one.
  - **`504`** — salt raises it from `SaltClientTimeout`, thrown *after* the job
    was sent and the gather timed out. The job is running.
  - read timeouts, resets, EOF — all can follow a fully written request.
- **The async engine does better than retrying: it reconciles.** `Run`/`Submit`
  mint the jid before sending, so on an ambiguous failure they ask
  `/jobs/<jid>` whether the job exists and either adopt it or re-send — no
  guessing, and a job proven never to have run is re-sent even if it is wildly
  non-idempotent. See [async.md](async.md) §C.1–C.2. That covers the common
  case without any opt-in.
- Declare a command safe to re-send with **`Command.ResendOnUnknown`** /
  **`JobSpec.ResendOnUnknown`**, or fleet-wide with **`WithResendOnUnknown()`**.
  This is consulted **only** where reconciliation can't help: a client type that
  takes no jid (`runner`, `wheel`), or a master too unreachable to ask. The
  zero value is the careful one — if papurika cannot tell whether a command ran,
  it does not run it again. A multi-command POST is one request, so it is
  repeatable only when every command in it is.

**Validate the lowstate before sending.** cherrypy answers a lowstate missing
`fun`/`tgt` with HTTP 500 and an opaque `"An unexpected error occurred"` — byte
for byte what it returns for an internal failure. The two are indistinguishable
from the response, so papurika rules the first out from the inside: the rendered
lowstate is checked for `client`/`fun`/`tgt` before the request leaves. A caller
gets `ErrMalformed` naming the missing field, and a `500` that *does* come back
is honestly attributable to the server (`ErrServerError`, distinct from
`ErrMalformed`).

**Automatic re-auth on authn-401** (tokens expire; unpatched 3008 evicts them —
`cherrypy_learnings.md` token-eviction bug):

```
Do(req):
  attach X-Auth-Token
  resp := httpc.Do(req)
  if classify(resp) == ErrAuthFailure && !req.reauthed:
      tok := reauth(ctx)        // single-flight under mu; one /login, even under a burst of 401s
      retry req once with tok
  // ErrAuthDenied (authz) is NOT retried — re-login can't grant a missing perm.
```

Guards: re-auth **exactly once** per request (a `reauthed` flag prevents loops); a
mutex so concurrent 401s collapse into one `/login`. Distinguishing **authn (retry)
from authz (don't)** is essential — and on tornado authz is even a 200, so the
classifier, not the status code, makes the call.

**salt-api specifics baked in:** session token in `X-Auth-Token` on `/`, `/jobs`,
`/events`; `/run` (`ExecStateless`) self-authenticates per request with the eauth
trio **or** a raw eauth token from the `auth.mk_token` runner — **never** the
`/login` session token (cherrypy 401s it); `POST /minions` returns **202** (treated
as success).

---

## E. Config, secrets, token cache (`config`, `internal/tokencache`)

Public `config` package so the CLI and third parties resolve identically.

```go
package config

type Config struct {
    DefaultProfile string             `toml:"default_profile"`
    Profiles       map[string]Profile `toml:"profiles"`
}

type Profile struct {
    MasterURL       string `toml:"master_url"`        // https://master:8000
    User            string `toml:"user"`
    Eauth           string `toml:"eauth"`             // pam | file | ldap | ...
    PasswordCommand string `toml:"password_command"`  // PREFERRED: exec, secret on stdout
    Password        string `toml:"password"`          // DISCOURAGED plaintext fallback
    VerifySSL       *bool  `toml:"verify_ssl"`        // default true
    CACert          string `toml:"ca_cert"`           // optional pinned CA
    Timeout         string `toml:"timeout"`           // client wall-clock budget, e.g. "120s"
}

func Load(paths ...string) (*Config, error)
func (c *Config) Resolve(profile string, ov Overrides) (Profile, error)
```

Example `config.toml`:

```toml
default_profile = "prod"

[profiles.prod]
master_url       = "https://salt.example.com:8000"
user             = "me"
eauth            = "file"
password_command = ["pass", "show", "salt/prod"]   # secret never in this file

[profiles.gcp]
master_url       = "http://salt-master.us-west1.example.com:8000"
user             = "me"
eauth            = "file"
password_command = ["pass", "show", "salt/gcp"]
```

**XDG paths** (fixes pepper §2.10):
- Config: `$XDG_CONFIG_HOME/papurika/config.toml` (fallback `~/.config/papurika/config.toml`); legacy `~/.pepperrc`-style path read if present, with a deprecation warning.
- Token cache: `$XDG_CACHE_HOME/papurika/tokens/` (fallback `~/.cache/papurika/tokens/`).

**Precedence:** flags > env (`PAPURIKA_*`) > config file (selected profile) > defaults.

**Secrets — exec helper** (fixes pepper §2.9, à la git `credential.helper` / mbsync
`PassCmd`):

```go
type SecretSource interface { Secret(ctx context.Context) (string, error) }

type StaticSecret string                    // discouraged; from plaintext `password`
func (s StaticSecret) Secret(context.Context) (string, error) { return string(s), nil }

type CommandSecret struct{ Argv []string }  // run argv; secret = trimmed first line of stdout
func (c CommandSecret) Secret(ctx context.Context) (string, error) // exec.CommandContext
```

If `password_command` is set → `CommandSecret` (secret can come from a password
manager, OS keychain, GPG file; the helper may prompt or require a touch assertion).
Else `Password` → `StaticSecret` (warn: plaintext). Resolved lazily at `Login`.

**Token cache keyed by endpoint** (fixes pepper#205):

```go
package tokencache

func Key(masterURL, user, eauth string) string  // sha256(masterURL|user|eauth)[:16]

type Store interface {
    Get(ctx context.Context, key string) (Token, bool, error)
    Put(ctx context.Context, key string, t Token) error
}

type Token struct {
    Token  string    `json:"token"`
    Expire time.Time `json:"expire"`  // from /login; checked before reuse
    Perms  []string  `json:"perms,omitempty"`
}

// FileStore: one file per key under $XDG_CACHE_HOME/papurika/tokens/<key>.json,
// written 0600 (dir 0700). Different profiles/masters never collide.
type FileStore struct{ dir string }
```

The client checks `Token.Expire` before reuse and re-logs in if expired — belt and
suspenders alongside the 401 re-auth (§D), since the eviction bug can drop a token
before its nominal expiry.

---

## Open questions

1. **macOS Keychain** — file `FileStore` (0600) is the default token store; do we want
   an opt-in Keychain-backed `Store` on macOS? (The `Store` interface already allows it.)
2. **Retry of `test.ping`-class reads** — should we ship a small allowlist of known
   side-effect-free functions that are retried even over `POST /`, or leave that
   entirely to `WithResendOnUnknown`? *(Partly settled: `WithResendOnUnknown`
   is implemented, and the provably-unexecuted failures are now retried for every
   command POST without any opt-in. An allowlist is still open — and is less
   pressing now that the common transient cases are covered by default.)*
3. ~~**Reconciling an ambiguous submit**~~ — **settled and implemented**; see
   [async.md](async.md) §C.1–C.2. The engine mints the jid, reconciles against
   `/jobs/<jid>`, and adopts or re-sends. Still open underneath it: `runner` and
   `wheel` reach the master via `cmd_sync`, which takes no client jid, so those
   paths cannot be reconciled — should they grow an equivalent, or stay
   send-once?
