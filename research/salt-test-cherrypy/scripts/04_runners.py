# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Runner client: master-side functions (no tgt).

The ops bread-and-butter discovery/inspection runners: cache.grains (fast,
cache-only host discovery), manage.status (up/down — pings minions),
manage.up, jobs.list_jobs, jobs.lookup_jid. Also runner_async to show a runner
returning a jid.
"""
import _common as c

token = c.login()

# cache.grains — read grains straight from the master cache (no minion traffic).
ls = [{"client": "runner", "fun": "cache.grains", "kwarg": {"tgt": "*"}}]
c.record("04a_runner_cache_grains", ls, c.post(token, ls))

# manage.status — which minions are up vs down (this one DOES ping minions).
ls = [{"client": "runner", "fun": "manage.status"}]
c.record("04b_runner_manage_status", ls, c.post(token, ls, timeout=60))

# manage.up — just the list of responsive minions.
ls = [{"client": "runner", "fun": "manage.up"}]
c.record("04c_runner_manage_up", ls, c.post(token, ls, timeout=60))

# jobs.list_jobs — the master's job cache (richer than GET /jobs).
ls = [{"client": "runner", "fun": "jobs.list_jobs"}]
c.record("04d_runner_jobs_list_jobs", ls, c.post(token, ls))

# runner_async — fire a runner and get a jid back instead of the result.
ls = [{"client": "runner_async", "fun": "manage.status"}]
c.record("04e_runner_async", ls, c.post(token, ls))
