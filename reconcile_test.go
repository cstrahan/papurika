package papurika

import (
	"context"
	"encoding/json"
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

// reconcileServer fakes a master whose submit fails ambiguously the first time.
// jobLookup decides what GET /jobs/<jid> reports, which is the evidence
// reconciliation turns on.
type reconcileServer struct {
	submits   atomic.Int32
	lookups   atomic.Int32
	seenJIDs  []string
	failFirst int    // how many submits fail ambiguously
	jobStatus int    // status for GET /jobs/<jid>
	jobBody   string // body for GET /jobs/<jid>
}

func (rs *reconcileServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch {
		case r.URL.Path == "/login":
			io.WriteString(w, loginBody("TOK1"))

		case strings.HasPrefix(r.URL.Path, "/jobs/"):
			rs.lookups.Add(1)
			w.WriteHeader(rs.jobStatus)
			io.WriteString(w, rs.jobBody)

		case r.URL.Path == "/":
			b, _ := io.ReadAll(r.Body)
			var low []map[string]any
			_ = json.Unmarshal(b, &low)
			jid, _ := low[0]["jid"].(string)
			rs.seenJIDs = append(rs.seenJIDs, jid)
			n := rs.submits.Add(1)
			if int(n) <= rs.failFirst {
				// 500: well-formed request, server-side failure. Ambiguous —
				// the master may or may not have published.
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "An unexpected error occurred")
				return
			}
			fmt.Fprintf(w, `{"return":[{"jid":%q,"minions":["m1"]}]}`, jid)

		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reconcileClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithHTTPClient(srv.Client()),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithEauth("pam"),
		WithTokenCache(tokencache.NewMemStore()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
	}
	c, err := New(srv.URL, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const jobFound = `{"info":[{"jid":"J","Function":"test.ping","Target":"*","Minions":["m1"],"Result":{}}],"return":[{}]}`
const jobMissing = `{"info":[{"jid":"J","Error":"Cannot contact returner or no job with this jid","Result":{}}],"return":[{}]}`

// The submit is sent under a jid we chose, so a lost response is answerable.
func TestSubmitMintsAConformingJID(t *testing.T) {
	rs := &reconcileServer{jobStatus: 200, jobBody: jobMissing}
	c := reconcileClient(t, rs.start(t))

	sub, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "test.ping"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(rs.seenJIDs) != 1 || rs.seenJIDs[0] == "" {
		t.Fatalf("no jid was sent in the lowstate: %v", rs.seenJIDs)
	}
	sent := rs.seenJIDs[0]
	if !looksLikeJID(sent) {
		t.Errorf("minted jid %q does not match salt's shape", sent)
	}
	if !strings.Contains(sent, "_") {
		t.Errorf("minted jid %q has no random suffix; bare timestamps collide silently", sent)
	}
	if got := sub.JIDs(); len(got) != 1 || got[0] != sent {
		t.Errorf("returned jids = %v, want the one we sent (%s)", got, sent)
	}
}

// The job cache says the job never existed, which proves _send_pub never ran.
// Re-sending is then safe for ANY command, declared idempotent or not.
func TestReconcileResendsWhenJobProvablyNeverRan(t *testing.T) {
	rs := &reconcileServer{failFirst: 1, jobStatus: 200, jobBody: jobMissing}
	c := reconcileClient(t, rs.start(t))

	sub, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "state.highstate"})
	if err != nil {
		t.Fatalf("Submit: %v (a job proven not to have run must be re-sent)", err)
	}
	if n := rs.submits.Load(); n != 2 {
		t.Errorf("submits = %d, want 2 (original + re-send)", n)
	}
	if n := rs.lookups.Load(); n != 1 {
		t.Errorf("job lookups = %d, want 1 (reconcile exactly once)", n)
	}
	if len(sub.JIDs()) != 1 {
		t.Fatalf("jids = %v, want exactly one", sub.JIDs())
	}
	// Same jid both times: the work stays recoverable under one id.
	if rs.seenJIDs[0] != rs.seenJIDs[1] {
		t.Errorf("re-send used a different jid (%s then %s)", rs.seenJIDs[0], rs.seenJIDs[1])
	}
}

// The job cache HAS the job: it was published and may be running. Adopting it
// is the only safe move — re-sending would execute the work twice.
func TestReconcileAdoptsWhenJobAlreadyExists(t *testing.T) {
	rs := &reconcileServer{failFirst: 1, jobStatus: 200, jobBody: jobFound}
	c := reconcileClient(t, rs.start(t))

	sub, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "state.highstate"})
	if err != nil {
		t.Fatalf("Submit: %v (an existing job should be adopted, not failed)", err)
	}
	if n := rs.submits.Load(); n != 1 {
		t.Errorf("submits = %d, want 1 — the job existed, re-sending would double-execute", n)
	}
	if len(sub.JIDs()) != 1 || sub.JIDs()[0] != rs.seenJIDs[0] {
		t.Errorf("jids = %v, want the adopted jid %s", sub.JIDs(), rs.seenJIDs[0])
	}
	if got := sub.Jobs[0].Targeted; len(got) != 1 || got[0] != "m1" {
		t.Errorf("adopted job targeted = %v, want [m1] from the job cache", got)
	}
}

// Reconciliation itself fails: we cannot tell whether the job ran. Default is
// to stop and hand back the jid, NOT to guess.
func TestReconcileUnknownDoesNotResendByDefault(t *testing.T) {
	rs := &reconcileServer{failFirst: 1, jobStatus: 500, jobBody: "master is down"}
	c := reconcileClient(t, rs.start(t))

	_, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "system.reboot"})
	if err == nil {
		t.Fatal("Submit succeeded; an unknowable outcome must not be reported as success")
	}
	if n := rs.submits.Load(); n != 1 {
		t.Errorf("submits = %d, want 1 — we could not prove the first one didn't run", n)
	}
	// The jid must survive in the error: it is the only handle on work that
	// may be running right now.
	if sent := rs.seenJIDs[0]; !strings.Contains(err.Error(), sent) {
		t.Errorf("err = %q, want it to carry the jid %s", err, sent)
	}
}

// ...unless the caller declared the command safe to repeat.
func TestReconcileUnknownResendsWhenDeclaredIdempotent(t *testing.T) {
	rs := &reconcileServer{failFirst: 1, jobStatus: 500, jobBody: "master is down"}
	c := reconcileClient(t, rs.start(t))

	_, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "test.ping", Idempotent: true})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if n := rs.submits.Load(); n != 2 {
		t.Errorf("submits = %d, want 2 (declared idempotent, so re-sending is allowed)", n)
	}
}

// The fleet-wide option is equivalent to marking every command idempotent.
func TestReconcileUnknownResendsWithClientOption(t *testing.T) {
	rs := &reconcileServer{failFirst: 1, jobStatus: 500, jobBody: "master is down"}
	c := reconcileClient(t, rs.start(t), WithRetryNonIdempotent())

	if _, err := c.Submit(context.Background(), JobSpec{Target: "*", Fun: "test.ping"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if n := rs.submits.Load(); n != 2 {
		t.Errorf("submits = %d, want 2", n)
	}
}

// A request salt understood and rejected is terminal: nothing was published, so
// there is nothing to reconcile and we must not burn a lookup on it.
func TestTerminalFailuresAreNotReconciled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"authz denied", http.StatusUnauthorized, "Authorization error occurred."},
		{"malformed", http.StatusBadRequest, "Invalid JSON document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Server", "CherryPy/18")
				if r.URL.Path == "/login" {
					io.WriteString(w, loginBody("TOK1"))
					return
				}
				if strings.HasPrefix(r.URL.Path, "/jobs/") {
					lookups.Add(1)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			if _, err := reconcileClient(t, srv).Submit(context.Background(),
				JobSpec{Target: "*", Fun: "test.ping"}); err == nil {
				t.Fatal("Submit succeeded, want an error")
			}
			if n := lookups.Load(); n != 0 {
				t.Errorf("job lookups = %d, want 0 — the request never reached the publish path", n)
			}
		})
	}
}

func TestIsAmbiguousSubmit(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("%w: boom", ErrServerError), true},
		{fmt.Errorf("%w: reset", ErrTransport), true},
		{fmt.Errorf("%w", ErrAuthDenied), false},
		{fmt.Errorf("%w", ErrMalformed), false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{errors.New("something else"), false},
	} {
		if got := isAmbiguousSubmit(tc.err); got != tc.want {
			t.Errorf("isAmbiguousSubmit(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestLooksLikeJID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"20261021120000000001", true},
		{"20261021120000000001_abc123", true},
		{"2026102112000000000", false},   // 19 digits
		{"202610211200000000019", false}, // 21 digits, no underscore
		{"2026102112000000000a", false},
		{"hello-world", false},
		{"", false},
		{"20261021120000000001_", false}, // empty suffix
	} {
		if got := looksLikeJID(tc.in); got != tc.want {
			t.Errorf("looksLikeJID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A hand-set jid that salt would silently accept but then mis-handle (StartTime,
// jid-ordered listings) is rejected before it reaches the wire.
func TestMalformedJIDRejectedBeforeSending(t *testing.T) {
	rs := &reconcileServer{jobStatus: 200, jobBody: jobMissing}
	c := reconcileClient(t, rs.start(t))

	_, err := c.Exec(context.Background(), Local("*", "test.ping").withJID("not-a-jid"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if n := rs.submits.Load(); n != 0 {
		t.Errorf("sent %d requests; a malformed jid must not reach the wire", n)
	}
}

func (c Command) withJID(j string) Command { c.JID = j; return c }
