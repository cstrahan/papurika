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

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

// fastPolls shrinks the gather poll cadence for tests and restores it after.
func fastPolls(t *testing.T) {
	t.Helper()
	op, ob := initialPollDelay, pollBackoffMax
	initialPollDelay = 20 * time.Millisecond
	pollBackoffMax = 40 * time.Millisecond
	t.Cleanup(func() { initialPollDelay, pollBackoffMax = op, ob })
}

// engineServer fakes login + local_async submit + /jobs/<jid> polling. /events
// 404s, exercising the polling fallback. jobReturns controls what each /jobs poll
// reports (called per poll).
func engineServer(t *testing.T, jid string, targeted []string, jobReturns func(poll int) map[string]string) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch {
		case r.URL.Path == "/login":
			io.WriteString(w, loginBody("TOK1"))
		case r.URL.Path == "/":
			// local_async submit.
			fmt.Fprintf(w, `{"return":[{"jid":%q,"minions":%s}]}`, jid, jsonList(targeted))
		case r.URL.Path == "/jobs/"+jid:
			n := int(polls.Add(1))
			rets := jobReturns(n)
			io.WriteString(w, jobBody(jid, targeted, rets))
		case r.URL.Path == "/events":
			http.Error(w, "no sse in test", http.StatusNotFound)
		default:
			http.Error(w, "nope", 404)
		}
	}))
}

func jsonList(ss []string) string {
	out := "["
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%q", s)
	}
	return out + "]"
}

// jobBody builds a GET /jobs/<jid> response: info[0].Result holds returns.
func jobBody(jid string, minions []string, rets map[string]string) string {
	result := "{"
	first := true
	for id, val := range rets {
		if !first {
			result += ","
		}
		first = false
		result += fmt.Sprintf("%q:{\"return\":%s,\"retcode\":0,\"success\":true}", id, val)
	}
	result += "}"
	return fmt.Sprintf(`{"info":[{"jid":%q,"Function":"test.ping","Minions":%s,"Result":%s}],"return":[%s]}`,
		jid, jsonList(minions), result, result)
}

func engineClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL,
		WithHTTPClient(srv.Client()),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithTokenCache(tokencache.NewMemStore()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunGathersViaPolling(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000001"
	targeted := []string{"m1", "m2"}
	// Poll 1: only m1 back. Poll 2+: both back.
	srv := engineServer(t, jid, targeted, func(poll int) map[string]string {
		if poll < 2 {
			return map[string]string{"m1": "true"}
		}
		return map[string]string{"m1": "true", "m2": "true"}
	})
	defer srv.Close()

	c := engineClient(t, srv)
	res, err := c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"}, WithDeadline(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	cr := res.Commands[0]
	if cr.JID != jid {
		t.Errorf("jid = %q, want %q", cr.JID, jid)
	}
	if !cr.Complete || cr.Reason != ReasonAllReturned {
		t.Errorf("not complete: complete=%v reason=%v", cr.Complete, cr.Reason)
	}
	if got := res.Responders(); len(got) != 2 {
		t.Errorf("responders = %v, want 2", got)
	}
	if got := res.NonResponders(); len(got) != 0 {
		t.Errorf("nonresponders = %v, want 0", got)
	}
}

func TestRunDeadlinePartialAndJID(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000002"
	targeted := []string{"m1", "m2"}
	// m2 never returns -> the run must hit its deadline and report partial.
	srv := engineServer(t, jid, targeted, func(poll int) map[string]string {
		return map[string]string{"m1": "true"}
	})
	defer srv.Close()

	c := engineClient(t, srv)
	res, err := c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"}, WithDeadline(150*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	// Partial result must be non-nil and carry the jid + the one responder.
	if res == nil || len(res.Commands) == 0 {
		t.Fatal("expected partial result, got nil")
	}
	if res.JIDs()[0] != jid {
		t.Errorf("jid lost: %v", res.JIDs())
	}
	if got := res.Responders(); len(got) != 1 || got[0] != "m1" {
		t.Errorf("responders = %v, want [m1]", got)
	}
	if got := res.NonResponders(); len(got) != 1 || got[0] != "m2" {
		t.Errorf("nonresponders = %v, want [m2]", got)
	}
}

func TestRunNoMatch(t *testing.T) {
	// local_async to a target matching nothing returns {"return":[{}]} (no jid).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		if r.URL.Path == "/login" {
			io.WriteString(w, loginBody("TOK1"))
			return
		}
		io.WriteString(w, `{"return":[{}]}`)
	}))
	defer srv.Close()
	c := engineClient(t, srv)

	res, err := c.Run(context.Background(), JobSpec{Target: "ghost", Fun: "test.ping"})
	if err != nil {
		t.Fatalf("no-match should not be an error, got %v", err)
	}
	if len(res.Commands) != 1 || len(res.Commands[0].Minions) != 0 {
		t.Fatalf("expected one empty command, got %+v", res.Commands)
	}
	if len(res.Responders()) != 0 || len(res.NonResponders()) != 0 {
		t.Errorf("expected no responders/non-responders")
	}
}

func TestRunSettlesDownMinion(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000099"
	targeted := []string{"m1", "m2"}
	// m2 never returns. With no hard deadline, the gather window should settle:
	// m2 becomes a non-responder and the run completes NORMALLY (not a timeout).
	srv := engineServer(t, jid, targeted, func(int) map[string]string {
		return map[string]string{"m1": "true"}
	})
	defer srv.Close()
	c := engineClient(t, srv)

	res, err := c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
		WithSaltTimeout(30*time.Millisecond), WithGatherTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("settle should be a normal completion, got err %v", err)
	}
	cr := res.Commands[0]
	if !cr.Complete || cr.Reason != ReasonSettled {
		t.Errorf("complete=%v reason=%v, want settled", cr.Complete, cr.Reason)
	}
	if got := res.Responders(); len(got) != 1 || got[0] != "m1" {
		t.Errorf("responders = %v, want [m1]", got)
	}
	if got := res.NonResponders(); len(got) != 1 || got[0] != "m2" {
		t.Errorf("nonresponders = %v, want [m2]", got)
	}
}

func TestRunFindJobWaitsForRunningMinion(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000199"
	targeted := []string{"m1", "m2"}
	// m1 returns; m2 never returns via /jobs BUT saltutil.find_job reports it is
	// still running the job. The gather must keep waiting on m2 (not settle it as
	// no-response); with no returns forthcoming, the run ends only at the deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch r.URL.Path {
		case "/login":
			io.WriteString(w, loginBody("TOK1"))
		case "/events":
			http.Error(w, "no sse", http.StatusNotFound)
		case "/jobs/" + jid:
			io.WriteString(w, jobBody(jid, targeted, map[string]string{"m1": "true"}))
		case "/":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "saltutil.find_job") {
				io.WriteString(w, fmt.Sprintf(`{"return":[{"m2":{"jid":%q,"pid":1}}]}`, jid)) // m2 still running
			} else {
				fmt.Fprintf(w, `{"return":[{"jid":%q,"minions":%s}]}`, jid, jsonList(targeted))
			}
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := engineClient(t, srv)

	res, err := c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
		WithSaltTimeout(20*time.Millisecond), WithGatherTimeout(20*time.Millisecond),
		WithDeadline(300*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded (find_job should keep m2 alive, not settle it)", err)
	}
	cr := res.Commands[0]
	if cr.Reason != ReasonDeadline {
		t.Errorf("reason = %v, want Deadline (m2 must not be settled while find_job says it's running)", cr.Reason)
	}
	if cr.Minions["m2"].Status != StatusPending {
		t.Errorf("m2 status = %v, want pending (alive, still running the job)", cr.Minions["m2"].Status)
	}
	if got := res.Responders(); len(got) != 1 || got[0] != "m1" {
		t.Errorf("responders = %v, want [m1]", got)
	}
}

func TestSubmitFireAndForget(t *testing.T) {
	jid := "20260627000000000003"
	srv := engineServer(t, jid, []string{"m1"}, func(int) map[string]string { return nil })
	defer srv.Close()
	c := engineClient(t, srv)

	sub, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "state.highstate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.JIDs()) != 1 || sub.JIDs()[0] != jid {
		t.Errorf("submit jids = %v, want [%s]", sub.JIDs(), jid)
	}
}

func TestWatchReturnsImmediatelyIfDone(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000004"
	targeted := []string{"m1", "m2"}
	// Already complete on the very first /jobs snapshot.
	srv := engineServer(t, jid, targeted, func(int) map[string]string {
		return map[string]string{"m1": "true", "m2": "true"}
	})
	defer srv.Close()
	c := engineClient(t, srv)

	start := time.Now()
	out, err := c.Watch(context.Background(), []string{jid}, WatchAllReturned, WithDeadline(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("watch on a done job should return promptly, took %v", time.Since(start))
	}
	if r := out[jid]; r == nil || len(r.Responders()) != 2 {
		t.Errorf("watch result = %+v, want 2 responders", out[jid])
	}
}

// A deadline that expires *during* the saltutil.find_job liveness probe must be
// reported as a deadline (exit 124), not quietly downgraded to "these minions
// didn't respond". A probe we never got an answer to is not evidence of death.
func TestRunDeadlineDuringFindJobIsNotSettled(t *testing.T) {
	fastPolls(t)
	jid := "20260627000000000200"
	targeted := []string{"m1", "m2"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch r.URL.Path {
		case "/login":
			io.WriteString(w, loginBody("TOK1"))
		case "/events":
			http.Error(w, "no sse", http.StatusNotFound)
		case "/jobs/" + jid:
			io.WriteString(w, jobBody(jid, targeted, map[string]string{"m1": "true"}))
		case "/":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "saltutil.find_job") {
				<-r.Context().Done() // hang until the run's deadline cuts the probe off
				return
			}
			fmt.Fprintf(w, `{"return":[{"jid":%q,"minions":%s}]}`, jid, jsonList(targeted))
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := engineClient(t, srv)

	res, err := c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
		WithSaltTimeout(20*time.Millisecond), WithGatherTimeout(20*time.Millisecond),
		WithDeadline(300*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded (an aborted probe must not settle minions)", err)
	}
	cr := res.Commands[0]
	if cr.Reason != ReasonDeadline {
		t.Errorf("reason = %v, want Deadline", cr.Reason)
	}
	if cr.Minions["m2"].Status != StatusPending {
		t.Errorf("m2 status = %v, want pending (the probe never answered)", cr.Minions["m2"].Status)
	}
}
