# research/

Throwaway-but-worth-keeping experiments backing the design of the papurika
salt-api client. Each subdirectory is a self-contained harness: a Dockerized
Salt master+minion, a set of `uv`-runnable probe scripts, the verbatim
request/response captures they produced, and a write-up of what we learned.

| dir | what |
|-----|------|
| [`salt-test-cherrypy/`](salt-test-cherrypy/) | Hands-on exploration of the `rest_cherrypy` netapi (the module prod runs). See its `cherrypy_learnings.md`. |
| [`salt-test-tornado/`](salt-test-tornado/) | Same treatment for `rest_tornado`, framed as a diff against cherrypy. See its `tornado_learnings.md`. |
| [`salt-test-gpg-pillar/`](salt-test-gpg-pillar/) | Cross-cutting unhappy-path: cold GPG-encrypted-pillar decryption (both modules, one master). See its `gpg_pillar_learnings.md`. |
| [`minion-status-and-liveness.md`](minion-status-and-liveness.md) | How salt's `manage.*` status tools decide liveness (live ping vs cache), the 3008.1 `manage.status` bug, and what the client should use instead. |

Headline cross-cutting finding: the two netapi modules are **not
interchangeable**. `rest_tornado` implements only `local`/`local_async`/
`runner`/`runner_async` (hardcoded) — no `wheel` (key management), no
`local_batch`/`local_subset` (batching), no `ssh`; requests for those get
`HTTP 400`. Several response shapes also differ (`/jobs/<jid>`, `/minions`
status, error/no-response signaling). Details in `salt-test-tornado/tornado_learnings.md`.

The harnesses are intentionally **self-contained** (each carries its own
scripts, demo states, and salt patch) rather than sharing a `common/` dir. The
scaffolding is largely netapi-agnostic, but keeping them independent let the
tornado probes diverge to expose tornado-specific behavior (e.g. dropping the
unsupported-client scripts, adding a `10_unsupported_clients` capture) without
coupling — which paid off. A shared `common/` could still be factored later if
the duplication becomes a maintenance burden.

These spin up local Docker containers and apply a salt source patch in-place;
they are developer tooling, not part of the shipped client.
