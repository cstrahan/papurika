package papurika

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

// fastRetry shrinks the backoff so retry tests don't sleep for seconds.
func fastRetry(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	base := []Option{
		WithHTTPClient(srv.Client()),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithEauth("pam"),
		WithTokenCache(tokencache.NewMemStore()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
	}
	c, err := New(srv.URL, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// statusServer answers /login normally and every command POST with status.
func statusServer(t *testing.T, posts *atomic.Int32, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		if r.URL.Path == "/login" {
			io.WriteString(w, loginBody("TOK1"))
			return
		}
		posts.Add(1)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

// A command POST is a job submission. Salt has no idempotency key, so on any
// failure that might mean "the master took it and is running it", re-sending
// starts a second execution under a jid nobody is watching. These statuses are
// all ambiguous and must be sent exactly once.
func TestCommandPostNotRetriedOnAmbiguousFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		// why the status cannot prove the job never started
		reason string
	}{
		{"500", http.StatusInternalServerError, "the master may have crashed after publishing"},
		{"502", http.StatusBadGateway, "a proxy failure says nothing about the upstream"},
		{"503", http.StatusServiceUnavailable, "salt raises it for SaltReqTimeoutError too, which may have published"},
		{"504", http.StatusGatewayTimeout, "salt raises it from SaltClientTimeout — the job IS running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := statusServer(t, &posts, tc.status, "nope")
			defer srv.Close()

			_, err := fastRetry(t, srv).Exec(context.Background(), Local("*", "test.ping"))
			if err == nil {
				t.Fatal("Exec succeeded, want an error")
			}
			if n := posts.Load(); n != 1 {
				t.Errorf("submitted %d times, want exactly 1 — %s", n, tc.reason)
			}
		})
	}
}

// salt-api never emits a 429 (there is none in salt/netapi), so one can only
// come from an intermediary that declined to forward the request. Nothing
// reached salt, so re-sending cannot double-execute.
func TestCommandPostRetriedOn429(t *testing.T) {
	var posts atomic.Int32
	srv := statusServer(t, &posts, http.StatusTooManyRequests, "slow down")
	defer srv.Close()

	_, err := fastRetry(t, srv).Exec(context.Background(), Local("*", "test.ping"))
	if err == nil {
		t.Fatal("Exec succeeded, want an error")
	}
	if n := posts.Load(); n != 3 {
		t.Errorf("submitted %d times, want 3 (the configured MaxAttempts)", n)
	}
}

// Opting in says "every command through this Client is safe to repeat", which
// widens retry to the ambiguous failures too.
func TestWithRetryNonIdempotentRetriesAmbiguousFailures(t *testing.T) {
	var posts atomic.Int32
	srv := statusServer(t, &posts, http.StatusServiceUnavailable, "busy")
	defer srv.Close()

	_, err := fastRetry(t, srv, WithRetryNonIdempotent()).Exec(context.Background(), Local("*", "test.ping"))
	if err == nil {
		t.Fatal("Exec succeeded, want an error")
	}
	if n := posts.Load(); n != 3 {
		t.Errorf("submitted %d times, want 3 — the caller declared repeats safe", n)
	}
}

// A connection that was refused never carried the request, so re-sending it is
// safe even for a submit. This is the master-is-restarting case.
func TestCommandPostRetriedWhenConnectionRefused(t *testing.T) {
	var posts atomic.Int32
	rt := &fakeRT{posts: &posts, err: syscall.ECONNREFUSED}

	c, err := New("http://salt.invalid:8000",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithEauth("pam"),
		WithTokenCache(tokencache.NewMemStore()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), Local("*", "test.ping")); err == nil {
		t.Fatal("Exec succeeded against a refused connection")
	}
	if n := posts.Load(); n != 3 {
		t.Errorf("attempted %d times, want 3 — a refused connection never carried the request", n)
	}
}

// By contrast, a failure that could have happened after the request was written
// must be attempted exactly once, even though it is also a transport error.
func TestCommandPostNotRetriedOnAmbiguousTransportError(t *testing.T) {
	var posts atomic.Int32
	rt := &fakeRT{posts: &posts, err: syscall.ECONNRESET}

	c, err := New("http://salt.invalid:8000",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithEauth("pam"),
		WithTokenCache(tokencache.NewMemStore()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), Local("*", "test.ping")); err == nil {
		t.Fatal("Exec succeeded against a reset connection")
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("attempted %d times, want 1 — a reset may have followed a written request", n)
	}
}

// fakeRT answers /login successfully and fails every command POST with err,
// counting the attempts.
type fakeRT struct {
	posts *atomic.Int32
	err   error
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/login" {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"application/json"}, "Server": {"CherryPy/18"}},
			Body:       io.NopCloser(strings.NewReader(loginBody("TOK1"))),
			Request:    r,
		}, nil
	}
	f.posts.Add(1)
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: f.err}
}

// A read timeout happens after the request was written, so the master may be
// running the job. It must not be treated as never-sent.
func TestAmbiguousTransportErrorsAreNotNeverSent(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		io.EOF,
		io.ErrUnexpectedEOF,
	} {
		if requestNeverSent(err) {
			t.Errorf("requestNeverSent(%v) = true, want false — it may have been written", err)
		}
	}
}

// cherrypy answers a malformed lowstate with the same opaque 500 as an internal
// failure, so papurika rules the first out client-side. A 500 that does come
// back is therefore the server's fault, not bad input.
func TestServerErrorIsNotMalformed(t *testing.T) {
	var posts atomic.Int32
	srv := statusServer(t, &posts, http.StatusInternalServerError, "An unexpected error occurred")
	defer srv.Close()

	_, err := fastRetry(t, srv).Exec(context.Background(), Local("*", "test.ping"))
	if !errors.Is(err, ErrServerError) {
		t.Errorf("err = %v, want ErrServerError", err)
	}
	if errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, must not be ErrMalformed — the request was well-formed", err)
	}
	var ae *APIError
	if errors.As(err, &ae); ae == nil || ae.StatusCode != 500 {
		t.Errorf("APIError not carried through: %+v", ae)
	}
}

// The validation exists so that a 500 can be blamed on the server. That only
// holds if papurika actually refuses to send a lowstate missing what salt
// requires — otherwise it would be indistinguishable from an internal failure.
func TestLowstateValidatedBeforeSending(t *testing.T) {
	var posts atomic.Int32
	srv := statusServer(t, &posts, http.StatusOK, `{"return":[{}]}`)
	defer srv.Close()
	c := fastRetry(t, srv)

	for _, tc := range []struct {
		name string
		cmd  Command
		want string
	}{
		{"missing fun", Command{Client: ClientLocal, Tgt: "*"}, "fun"},
		{"missing tgt", Command{Client: ClientLocal, Fun: "test.ping"}, "tgt"},
		{"missing client", Command{Fun: "test.ping", Tgt: "*"}, "client"},
		{"fun blanked via Extra", Local("*", "test.ping").withExtra("fun", ""), "fun"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Exec(context.Background(), tc.cmd)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
			if n := posts.Load(); n != 0 {
				t.Errorf("sent %d requests; an invalid lowstate must never reach the wire", n)
			}
		})
	}
}

// Runner and wheel act on the master and legitimately carry no tgt.
func TestRunnerNeedsNoTarget(t *testing.T) {
	if err := validateLowstate(Runner("manage.up").lowstate(true)); err != nil {
		t.Errorf("runner rejected: %v", err)
	}
	if err := validateLowstate(Wheel("key.list_all").lowstate(true)); err != nil {
		t.Errorf("wheel rejected: %v", err)
	}
}

func (c Command) withExtra(k string, v any) Command {
	c.Extra = map[string]any{k: v}
	return c
}
