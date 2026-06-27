# research/

Throwaway-but-worth-keeping experiments backing the design of the papurika
salt-api client. Each subdirectory is a self-contained harness: a Dockerized
Salt master+minion, a set of `uv`-runnable probe scripts, the verbatim
request/response captures they produced, and a write-up of what we learned.

| dir | what |
|-----|------|
| [`salt-test-cherrypy/`](salt-test-cherrypy/) | Hands-on exploration of the `rest_cherrypy` netapi (the module prod runs). See its `cherrypy_learnings.md`. |
| `salt-test-tornado/` | _(planned)_ same treatment for `rest_tornado`. |

The harnesses are intentionally **self-contained** (each carries its own
scripts, demo states, and salt patch) rather than sharing a `common/` dir. Much
of the scaffolding is netapi-agnostic and could be factored out once the
tornado harness exists and we can see what actually diverges — deferred until
then to avoid guessing at the wrong abstraction.

These spin up local Docker containers and apply a salt source patch in-place;
they are developer tooling, not part of the shipped client.
