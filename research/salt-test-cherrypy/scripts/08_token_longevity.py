# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Token longevity: does a single token survive past master maintenance sweeps?

Pre-patch on 3008 a token was swept within ~60s (TokenExpiredError) despite
token_expire=43200. Log in once, then reuse the SAME token across ~120s
(multiple sweeps). Records the per-sample outcome to out/08_token_longevity.json
so the result is comparable across versions (3006 should never evict early).
"""
import json
import time

import _common as c

token = c.login()
samples = [{"t": 0}]
r = c.post(token, [{"client": "local", "tgt": "*", "fun": "test.ping"}], timeout=20)
samples[0].update(status=r.status_code, ok=bool(r.status_code == 200 and r.json().get("return")))
print(f"t=0   -> HTTP {r.status_code}")

elapsed = 0
for wait in (40, 40, 40):  # ~120s total, crossing multiple maintenance sweeps
    time.sleep(wait)
    elapsed += wait
    r = c.post(token, [{"client": "local", "tgt": "*", "fun": "test.ping"}], timeout=20)
    ok = bool(r.status_code == 200 and r.json().get("return"))
    samples.append({"t": elapsed, "status": r.status_code, "ok": ok, "body": r.text[:200]})
    print(f"t=+{elapsed}s -> HTTP {r.status_code} ok={ok}")

result = {
    "token_len": len(token),
    "samples": samples,
    "survived_all": all(s.get("ok") for s in samples),
}
(c.OUT / "08_token_longevity.json").write_text(json.dumps(result, indent=2))
print("survived_all:", result["survived_all"], "-> out/08_token_longevity.json")
