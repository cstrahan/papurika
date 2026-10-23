package papurika

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// All payloads below are the shapes captured live against 3008.3 in
// research/salt-test-cherrypy/out/25a_runner_async_events.json and
// out/25b_runner_async_job_lookup.json.

const asyncJID = "20261023075159964896"

// runner_async submit: {"tag": "salt/run/<jid>", "jid": "<jid>"}
var runnerSubmitBody = fmt.Sprintf(`{"return":[{"tag":"salt/run/%s","jid":"%s"}]}`, asyncJID, asyncJID)

// The salt/run/<jid>/ret event payload, as EventStream hands it over (the
// inner "data" object).
const runnerRetEventData = `{"fun":"runner.jobs.active","jid":"` + asyncJID + `","user":"saltdev",
 "fun_args":[],"_stamp":"2026-10-23T07:52:00.241773+00:00",
 "return":{"20260627222818016951":{"Function":"cmd.run","Arguments":["sleep 120"],
   "Target":"*","Target-type":"glob","User":"saltdev",
   "StartTime":"2026, Jun 27 22:28:18.016951",
   "Running":[{"builtin.minion2":19816},{"builtin.minion":19815}],"Returned":[]}},
 "success":true}`

// GET /jobs/<jid> for a runner jid: the envelope sits at
// info[0].Result.master.return.
const runnerJobLookup = `{"info":[{"jid":"` + asyncJID + `","Function":"runner.jobs.active",
 "Arguments":[],"Target":"master","Target-type":"list","User":"saltdev",
 "StartTime":"2026, Oct 23 07:51:59.964896",
 "Result":{"master":{"return":` + runnerRetEventData + `}}}],
 "return":[{"master":` + runnerRetEventData + `}]}`

// asyncRunnerServer fakes the async runner flow. sse controls whether /events
// is available, which selects the event path or the polling backstop.
type asyncRunnerServer struct {
	sse       bool
	submits   atomic.Int32
	jobGets   atomic.Int32
	jobBody   string // GET /jobs/<jid>; empty => "not recorded yet"
	published chan struct{}
}

func (s *asyncRunnerServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	s.published = make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch {
		case r.URL.Path == "/login":
			io.WriteString(w, loginBody("TOK1"))

		case r.URL.Path == "/events":
			if !s.sse {
				http.Error(w, "no sse", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f, _ := w.(http.Flusher)
			if f != nil {
				f.Flush()
			}
			// Emit the return only once the job has actually been submitted,
			// mirroring the real ordering.
			select {
			case <-s.published:
			case <-r.Context().Done():
				return
			}
			fmt.Fprintf(w, "tag: salt/run/%s/ret\ndata: %s\n\n", asyncJID,
				strings.Join(strings.Fields(`{"tag":"salt/run/`+asyncJID+`/ret","data":`+runnerRetEventData+`}`), ""))
			if f != nil {
				f.Flush()
			}
			<-r.Context().Done()

		case strings.HasPrefix(r.URL.Path, "/jobs/"):
			s.jobGets.Add(1)
			if s.jobBody == "" {
				io.WriteString(w, `{"info":[{"jid":"`+asyncJID+`","Result":{}}],"return":[{}]}`)
				return
			}
			io.WriteString(w, s.jobBody)

		case r.URL.Path == "/":
			s.submits.Add(1)
			io.WriteString(w, runnerSubmitBody)
			close(s.published)

		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The point of the change: the runner's return arrives over the event bus, so
// no cherrypy worker is held for the duration of a fleet-wide fan-out.
func TestExecAsyncGathersFromTheEventBus(t *testing.T) {
	fastPolls(t)
	srv := &asyncRunnerServer{sse: true}
	c := newTestClient(t, srv.start(t))

	res, err := c.ExecAsync(context.Background(), Runner("jobs.active").asAsyncRunner(),
		WithDeadline(5*time.Second))
	if err != nil {
		t.Fatalf("ExecAsync: %v", err)
	}
	cr := res.Commands[0]
	if cr.JID != asyncJID {
		t.Errorf("jid = %q, want %q", cr.JID, asyncJID)
	}
	// Value must be the runner's return, unwrapped from the event envelope.
	m, ok := cr.Value.Map()
	if !ok || len(m) != 1 {
		t.Fatalf("value = %s, want the runner's return map", cr.Value.Raw())
	}
	if _, ok := m["20260627222818016951"]; !ok {
		t.Errorf("value = %v, want it keyed by the active jid", m)
	}
	if n := srv.submits.Load(); n != 1 {
		t.Errorf("submits = %d, want 1", n)
	}
}

// The event stream is lossy, so a missed /ret must not hang until the
// deadline. A runner jid IS resolvable through /jobs, which makes polling a
// real backstop.
func TestExecAsyncFallsBackToJobCache(t *testing.T) {
	fastPolls(t)
	srv := &asyncRunnerServer{sse: false, jobBody: runnerJobLookup}
	c := newTestClient(t, srv.start(t))

	res, err := c.ExecAsync(context.Background(), Runner("jobs.active").asAsyncRunner(),
		WithDeadline(5*time.Second))
	if err != nil {
		t.Fatalf("ExecAsync: %v (the job cache should have answered)", err)
	}
	if res.Commands[0].JID != asyncJID {
		t.Errorf("jid = %q, want %q", res.Commands[0].JID, asyncJID)
	}
	if _, ok := res.Commands[0].Value.Map(); !ok {
		t.Errorf("value = %s, want the runner's return map", res.Commands[0].Value.Raw())
	}
	if srv.jobGets.Load() == 0 {
		t.Error("expected at least one /jobs poll")
	}
}

// Never lose a jid: a deadline must still hand back the jid of work that is
// still running on the master.
func TestExecAsyncDeadlineKeepsTheJID(t *testing.T) {
	fastPolls(t)
	srv := &asyncRunnerServer{sse: false} // never answers
	c := newTestClient(t, srv.start(t))

	res, err := c.ExecAsync(context.Background(), Runner("jobs.active").asAsyncRunner(),
		WithDeadline(150*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if res == nil {
		t.Fatal("result is nil; the jid would be lost")
	}
	if res.Commands[0].JID != asyncJID {
		t.Errorf("jid = %q, want %q carried on the partial result", res.Commands[0].JID, asyncJID)
	}
	if res.Commands[0].Reason != ReasonDeadline {
		t.Errorf("reason = %v, want Deadline", res.Commands[0].Reason)
	}
}

func TestExecAsyncRejectsNonAsyncClients(t *testing.T) {
	srv := &asyncRunnerServer{}
	c := newTestClient(t, srv.start(t))

	for _, ct := range []ClientType{ClientRunner, ClientLocal, ClientWheel} {
		_, err := c.ExecAsync(context.Background(), Command{Client: ct, Fun: "x"})
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("client %q: err = %v, want ErrUnsupported", ct, err)
		}
	}
	if n := srv.submits.Load(); n != 0 {
		t.Errorf("sent %d requests for an unsupported client; want 0", n)
	}
}

func TestMasterEventPrefix(t *testing.T) {
	for ct, want := range map[ClientType]string{
		ClientRunnerAsync: "salt/run/",
		ClientWheelAsync:  "salt/wheel/",
	} {
		got, err := masterEventPrefix(ct)
		if err != nil || got != want {
			t.Errorf("masterEventPrefix(%q) = %q, %v; want %q", ct, got, err, want)
		}
	}
}

// salt reports success:true for a runner called with missing arguments, with
// the usage text as the return VALUE. Anything trusting the flag alone calls
// that a successful run.
func TestRunnerFailedCatchesWhatSuccessDoesNot(t *testing.T) {
	bad := NewRawValue([]byte(`"\nPassed invalid arguments: lookup_jid() missing 1 required positional argument: 'jid'\n\nUsage:\n"`))
	msg, failed := RunnerFailed(bad)
	if !failed {
		t.Fatal("RunnerFailed did not flag salt's invalid-arguments return")
	}
	if !strings.Contains(msg, "Passed invalid arguments") {
		t.Errorf("msg = %q, want the first line of the error", msg)
	}

	for _, ok := range []RawValue{
		NewRawValue([]byte(`{"jid": {}}`)),
		NewRawValue([]byte(`"all good"`)),
		NewRawValue([]byte(`true`)),
	} {
		if _, failed := RunnerFailed(ok); failed {
			t.Errorf("RunnerFailed(%s) = true, want false", ok.Raw())
		}
	}
}

// asAsyncRunner is test sugar for the client-type switch ActiveJobs makes.
func (c Command) asAsyncRunner() Command { c.Client = ClientRunnerAsync; return c }

// ActiveJobs through the async path still folds salt's
// Running: [{"m2": pid}, {"m1": pid}] into one map.
func TestActiveJobsOverAsyncRunner(t *testing.T) {
	fastPolls(t)
	srv := &asyncRunnerServer{sse: true}
	c := newTestClient(t, srv.start(t))

	active, err := c.ActiveJobs(context.Background())
	if err != nil {
		t.Fatalf("ActiveJobs: %v", err)
	}
	job, ok := active["20260627222818016951"]
	if !ok {
		t.Fatalf("job missing from %v", active)
	}
	if job.Function != "cmd.run" || job.Target != "*" {
		t.Errorf("metadata = %+v", job.JobMeta)
	}
	if len(job.Running) != 2 || job.Running["builtin.minion"] != 19815 || job.Running["builtin.minion2"] != 19816 {
		t.Errorf("Running = %v, want both minions with pids", job.Running)
	}
}

// An empty fleet-wide answer is "nothing running", not an error.
func TestActiveJobsEmptyOverAsyncRunner(t *testing.T) {
	fastPolls(t)
	empty := `{"info":[{"jid":"` + asyncJID + `","Result":{"master":{"return":` +
		`{"fun":"runner.jobs.active","jid":"` + asyncJID + `","return":{},"success":true}}}}],"return":[{}]}`
	srv := &asyncRunnerServer{sse: false, jobBody: empty}
	c := newTestClient(t, srv.start(t))

	active, err := c.ActiveJobs(context.Background())
	if err != nil {
		t.Fatalf("ActiveJobs: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active = %v, want empty", active)
	}
}

// Interrupting a call whose success type is a map must still yield the jid:
// the runner keeps going on the master, and without the jid it is unfindable.
// This is the "never lose a jid" rule on a return type with nowhere to put one.
func TestActiveJobsCancelledStillCarriesTheJID(t *testing.T) {
	fastPolls(t)
	srv := &asyncRunnerServer{sse: false} // submits, then never answers
	c := newTestClient(t, srv.start(t))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := c.ActiveJobs(ctx)
	if err == nil {
		t.Fatal("ActiveJobs succeeded, want a deadline error")
	}
	var je *JobError
	if !errors.As(err, &je) {
		t.Fatalf("err = %v (%T), want a *JobError carrying the jid", err, err)
	}
	if je.JID != asyncJID {
		t.Errorf("JobError.JID = %q, want %q", je.JID, asyncJID)
	}
	// The underlying cause must stay matchable.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to still unwrap to DeadlineExceeded", err)
	}
	// And printing it alone must not lose the jid.
	if !strings.Contains(err.Error(), asyncJID) {
		t.Errorf("err.Error() = %q, want it to name the jid", err.Error())
	}
}

// A failure before the job exists has no jid to carry, and must not invent one.
func TestJobErrorOnlyWrapsWhenThereIsAJID(t *testing.T) {
	if got := withJID("", context.Canceled); got != context.Canceled {
		t.Errorf("withJID with no jid = %v, want the error unchanged", got)
	}
	if got := withJID("j1", nil); got != nil {
		t.Errorf("withJID(nil error) = %v, want nil", got)
	}
}
