package papurika

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Verbatim from research/salt-test-cherrypy/out/04d_runner_jobs_list_jobs.json.
// Note the two Target shapes in one response: a glob job carries a string, a
// list-targeted job carries an ARRAY. Decoding Target as a string fails the
// whole listing — and papurika makes list-targeted jobs itself (every
// saltutil.find_job liveness probe is one), so any fleet it has run against
// accumulates them.
const jobsListMixedTargets = `{"return": [{
  "20260626233733357635": {"Function": "cmd.run", "Arguments": ["echo batched"],
    "Target": ["builtin.minion"], "Target-type": "list", "User": "saltdev",
    "StartTime": "2026, Jun 26 23:37:33.357635"},
  "20260626233740111111": {"Function": "test.ping", "Arguments": [],
    "Target": "*", "Target-type": "glob", "User": "saltdev",
    "StartTime": "2026, Jun 26 23:37:40.111111"}
}]}`

// Verbatim shape from GET /jobs/<jid> for a list-targeted job (captured live
// against 3008.3).
const jobDetailListTarget = `{"info": [{"jid": "20261023072324444951",
  "Function": "test.ping", "Arguments": [], "Target": ["builtin.minion"],
  "Target-type": "list", "User": "saltdev", "Minions": ["builtin.minion"],
  "StartTime": "2026, Oct 23 07:23:24.444951",
  "Result": {"builtin.minion": {"return": true, "retcode": 0, "success": true}}}],
  "return": [{"builtin.minion": true}]}`

func jobsServer(t *testing.T, path, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		if r.URL.Path == "/login" {
			io.WriteString(w, loginBody("TOK1"))
			return
		}
		if r.URL.Path == path || path == "*" {
			io.WriteString(w, body)
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJobsDecodesBothTargetShapes(t *testing.T) {
	c := newTestClient(t, jobsServer(t, "/jobs", jobsListMixedTargets))

	jobs, err := c.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v (a list-valued Target must not fail the listing)", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	if got := jobs["20260626233733357635"].Target; got != "builtin.minion" {
		t.Errorf("list Target = %q, want %q", got, "builtin.minion")
	}
	if got := jobs["20260626233740111111"].Target; got != "*" {
		t.Errorf("string Target = %q, want %q", got, "*")
	}
}

// /jobs/<jid> carries the same divergence, and this one sits in the gather and
// watch path: failing it means /jobs reconciliation silently never works for a
// list-targeted job.
func TestJobDetailDecodesListTarget(t *testing.T) {
	c := newTestClient(t, jobsServer(t, "/jobs/20261023072324444951", jobDetailListTarget))

	jr, err := c.Job(context.Background(), "20261023072324444951")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if jr.Meta.Target != "builtin.minion" {
		t.Errorf("Target = %q, want %q", jr.Meta.Target, "builtin.minion")
	}
	if m, ok := jr.Result.Minions["builtin.minion"]; !ok || m.Status != StatusResponded {
		t.Errorf("minion result not decoded: %+v", jr.Result.Minions)
	}
}

// An unrecognized Target shape must degrade, not fail the decode: one odd field
// should never take down a whole job listing.
func TestFlexTargetToleratesUnknownShapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"*"`, "*"},
		{`["a","b"]`, "a,b"},
		{`[]`, ""},
		{`null`, ""},
		{`{"weird":1}`, `{"weird":1}`},
		{`42`, "42"},
	} {
		var ft flexTarget
		if err := json.Unmarshal([]byte(tc.in), &ft); err != nil {
			t.Errorf("UnmarshalJSON(%s) errored: %v", tc.in, err)
			continue
		}
		if got := ft.String(); got != tc.want {
			t.Errorf("flexTarget(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole listing must survive a job whose Target is unparseable.
func TestJobsSurvivesAnOddTarget(t *testing.T) {
	body := `{"return": [{"20260101000000000001": {"Function": "x", "Target": {"n": 1}, "User": "u"}}]}`
	c := newTestClient(t, jobsServer(t, "/jobs", body))

	jobs, err := c.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v — an odd Target must not fail the listing", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if got := jobs["20260101000000000001"].Target; !strings.Contains(got, "n") {
		t.Errorf("Target = %q, want the raw text preserved", got)
	}
}
