package papurika

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hangingServer answers /login and then never answers anything else, so a
// request made against it can only end by the client's context expiring.
//
// The handler waits on an explicit release channel rather than on
// r.Context(): a client-side cancel does not reliably surface as a server-side
// request cancellation here, and a handler still parked in ServeHTTP makes
// httptest.Server.Close block forever.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			io.WriteString(w, loginBody("TOK1"))
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // LIFO: released before Close waits
	return srv
}

// A deadline that expires mid-request must stay matchable as
// context.DeadlineExceeded. The CLI maps a deadline to exit 124 and everything
// else transport-ish to 69, so collapsing the two reported "service
// unavailable" for a timeout the caller asked for.
func TestTransportPreservesDeadlineCause(t *testing.T) {
	srv := hangingServer(t)
	c := fastRetry(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := c.Exec(ctx, Local("*", "test.ping"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("want ErrTransport, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want context.DeadlineExceeded to stay reachable, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("a deadline must not look like a cancel: %v", err)
	}
}

// The same for an explicit cancel, which the CLI maps to exit 130 (SIGINT).
func TestTransportPreservesCancelCause(t *testing.T) {
	srv := hangingServer(t)
	c := fastRetry(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := c.Exec(ctx, Local("*", "test.ping"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("want ErrTransport, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled to stay reachable, got %v", err)
	}
}

// The cause must also reach the message. "transport error" alone told an
// operator nothing about why their command stopped.
func TestAPIErrorMessageNamesTheCause(t *testing.T) {
	e := causedError(ErrTransport, context.DeadlineExceeded, 0, "", "", ModuleUnknown)
	want := "transport error: context deadline exceeded"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// errors.As must keep working alongside the two-error Unwrap.
	var ae *APIError
	if !errors.As(error(e), &ae) {
		t.Error("errors.As(*APIError) broke")
	}
}
