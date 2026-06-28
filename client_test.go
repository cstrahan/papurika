package papurika

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

func loginBody(token string) string {
	exp := float64(time.Now().Add(time.Hour).Unix())
	return fmt.Sprintf(`{"return":[{"token":%q,"expire":%f,"perms":[".*"]}]}`, token, exp)
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL,
		WithHTTPClient(srv.Client()),
		WithCredentials("alice", config.StaticSecret("pw")),
		WithEauth("pam"),
		WithTokenCache(tokencache.NewMemStore()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginAndExec(t *testing.T) {
	var logins, execs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			logins.Add(1)
			var creds map[string]string
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &creds)
			if creds["username"] != "alice" || creds["password"] != "pw" || creds["eauth"] != "pam" {
				t.Errorf("bad login creds: %v", creds)
			}
			w.Header().Set("Server", "CherryPy/18")
			io.WriteString(w, loginBody("TOK1"))
		case "/":
			execs.Add(1)
			if got := r.Header.Get("X-Auth-Token"); got != "TOK1" {
				t.Errorf("X-Auth-Token = %q, want TOK1", got)
			}
			// Verify full_return was injected.
			body, _ := io.ReadAll(r.Body)
			if !bytesContains(body, `"full_return":true`) {
				t.Errorf("full_return not injected: %s", body)
			}
			io.WriteString(w, `{"return":[{"m1":{"ret":true,"retcode":0,"jid":"1"}}]}`)
		default:
			http.Error(w, "nope", 404)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Exec(context.Background(), Local("*", "test.ping"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Commands[0].Minions["m1"]
	if !ok || m.Status != StatusResponded || m.Retcode == nil || *m.Retcode != 0 {
		t.Fatalf("unexpected minion result: %+v", m)
	}
	if b, _ := m.Value.Bool(); !b {
		t.Errorf("expected ret true")
	}
	// Second exec should reuse the cached token (no second login).
	if _, err := c.Exec(context.Background(), Local("*", "test.ping")); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 1 {
		t.Errorf("logins = %d, want 1 (token should be cached)", logins.Load())
	}
	if execs.Load() != 2 {
		t.Errorf("execs = %d, want 2", execs.Load())
	}
}

func TestReauthOn401(t *testing.T) {
	var logins atomic.Int32
	var execAttempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch r.URL.Path {
		case "/login":
			n := logins.Add(1)
			io.WriteString(w, loginBody(fmt.Sprintf("TOK%d", n)))
		case "/":
			n := execAttempts.Add(1)
			// First attempt: the (stale) token is rejected. After re-auth, accept.
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `<html>Please log in</html>`)
				return
			}
			if r.Header.Get("X-Auth-Token") != "TOK2" {
				t.Errorf("retry used token %q, want TOK2", r.Header.Get("X-Auth-Token"))
			}
			io.WriteString(w, `{"return":[{"m1":true}]}`)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	res, err := c.Exec(context.Background(), Local("*", "test.ping"))
	if err != nil {
		t.Fatalf("expected success after re-auth, got %v", err)
	}
	if res.Commands[0].Minions["m1"].Status != StatusResponded {
		t.Errorf("unexpected: %+v", res.Commands[0].Minions["m1"])
	}
	if logins.Load() != 2 {
		t.Errorf("logins = %d, want 2 (initial + re-auth)", logins.Load())
	}
	if execAttempts.Load() != 2 {
		t.Errorf("exec attempts = %d, want 2", execAttempts.Load())
	}
}

func TestAuthzDeniedNotRetried(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		if r.URL.Path == "/login" {
			logins.Add(1)
			io.WriteString(w, loginBody("TOK1"))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `<html>Authorization error occurred.</html>`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Exec(context.Background(), Local("*", "cmd.run", "id"))
	if !errors.Is(err, ErrAuthDenied) {
		t.Fatalf("err = %v, want ErrAuthDenied", err)
	}
	// Authz denial must NOT trigger a re-auth loop (only the initial login).
	if logins.Load() != 1 {
		t.Errorf("logins = %d, want 1 (authz must not re-auth)", logins.Load())
	}
}

func TestExecStatelessUsesEauthTrio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" {
			t.Errorf("stateless should hit /run, got %s", r.URL.Path)
		}
		if r.Header.Get("X-Auth-Token") != "" {
			t.Errorf("/run must not send a session token")
		}
		var arr []map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &arr)
		if arr[0]["username"] != "alice" || arr[0]["password"] != "pw" || arr[0]["eauth"] != "pam" {
			t.Errorf("eauth trio not embedded: %v", arr[0])
		}
		io.WriteString(w, `{"return":[{"m1":true}]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.ExecStateless(context.Background(), Local("*", "test.ping")); err != nil {
		t.Fatal(err)
	}
}

func bytesContains(b []byte, s string) bool { return bodyContains(b, s) }
