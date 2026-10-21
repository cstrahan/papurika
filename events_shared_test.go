package papurika

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

// sharedEventsServer counts /events connections — the resource under test. Each
// one occupies a cherrypy worker for as long as it is held.
type sharedEventsServer struct {
	opens   atomic.Int32
	live    atomic.Int32
	jobGets atomic.Int32

	mu       sync.Mutex
	returned map[string]bool // jid -> report the job as complete
}

func (s *sharedEventsServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	s.returned = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch {
		case r.URL.Path == "/login":
			io.WriteString(w, loginBody("TOK1"))

		case r.URL.Path == "/events":
			s.opens.Add(1)
			s.live.Add(1)
			defer s.live.Add(-1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // hold it open like a real stream

		case strings.HasPrefix(r.URL.Path, "/jobs/"):
			s.jobGets.Add(1)
			jid := strings.TrimPrefix(r.URL.Path, "/jobs/")
			s.mu.Lock()
			done := s.returned[jid]
			s.mu.Unlock()
			if done {
				io.WriteString(w, jobBody(jid, []string{"m1"}, map[string]string{"m1": "true"}))
				return
			}
			io.WriteString(w, jobBody(jid, []string{"m1"}, nil))

		case r.URL.Path == "/":
			b, _ := io.ReadAll(r.Body)
			jid := extractJID(string(b))
			fmt.Fprintf(w, `{"return":[{"jid":%q,"minions":["m1"]}]}`, jid)

		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func extractJID(body string) string {
	const k = `"jid":"`
	i := strings.Index(body, k)
	if i < 0 {
		return "20260101000000000001"
	}
	rest := body[i+len(k):]
	return rest[:strings.IndexByte(rest, '"')]
}

func sharedClient(t *testing.T, srv *httptest.Server) *Client {
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

// The point of the exercise: concurrent jobs must share ONE /events connection,
// not open one each. Each connection holds a cherrypy worker thread.
func TestConcurrentGathersShareOneEventStream(t *testing.T) {
	fastPolls(t)
	srv := &sharedEventsServer{}
	c := sharedClient(t, srv.start(t))

	const jobs = 8
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
				WithSaltTimeout(30*time.Millisecond), WithGatherTimeout(30*time.Millisecond),
				WithDeadline(3*time.Second))
		}()
	}
	wg.Wait()

	if n := srv.opens.Load(); n != 1 {
		t.Errorf("/events connections = %d, want 1 for %d concurrent jobs", n, jobs)
	}
}

// ...and the shared connection must be released when the last job finishes, so
// an idle Client holds no worker hostage.
func TestSharedStreamClosesWhenLastJobFinishes(t *testing.T) {
	fastPolls(t)
	srv := &sharedEventsServer{}
	c := sharedClient(t, srv.start(t))

	_, _ = c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
		WithSaltTimeout(30*time.Millisecond), WithGatherTimeout(30*time.Millisecond),
		WithDeadline(3*time.Second))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.live.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := srv.live.Load(); n != 0 {
		t.Errorf("live /events connections = %d after the last job, want 0", n)
	}

	c.evMu.Lock()
	stream, refs := c.evStream, c.evRefs
	c.evMu.Unlock()
	if stream != nil || refs != 0 {
		t.Errorf("shared stream not cleared: stream=%v refs=%d", stream != nil, refs)
	}
}

// A later job must get a working stream after the shared one was closed.
func TestSharedStreamReopensAfterRelease(t *testing.T) {
	fastPolls(t)
	srv := &sharedEventsServer{}
	c := sharedClient(t, srv.start(t))

	for i := 0; i < 2; i++ {
		_, _ = c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
			WithSaltTimeout(30*time.Millisecond), WithGatherTimeout(30*time.Millisecond),
			WithDeadline(3*time.Second))
	}
	if n := srv.opens.Load(); n != 2 {
		t.Errorf("/events opens = %d, want 2 (one per sequential job)", n)
	}
}

// A stream shared across many short jobs outlives all of them, so anything a
// job registers and fails to remove leaks for the life of the Client.
func TestGatherLeavesNoSubscriptionsBehind(t *testing.T) {
	fastPolls(t)
	srv := &sharedEventsServer{}
	c := sharedClient(t, srv.start(t))

	// Hold one job open so the shared stream stays alive and inspectable.
	hold, release := c.acquireEvents(context.Background())
	if hold == nil {
		t.Fatal("could not open the shared stream")
	}
	defer release()

	for i := 0; i < 5; i++ {
		_, _ = c.Run(context.Background(), JobSpec{Target: "*", Fun: "test.ping"},
			WithSaltTimeout(30*time.Millisecond), WithGatherTimeout(30*time.Millisecond),
			WithDeadline(3*time.Second))
	}

	hold.mu.Lock()
	subs, recs := len(hold.subs), len(hold.recListeners)
	hold.mu.Unlock()
	if subs != 0 {
		t.Errorf("subscriptions left registered = %d, want 0", subs)
	}
	if recs != 0 {
		t.Errorf("reconnect listeners left registered = %d, want 0", recs)
	}
}

func TestReconnectsUnregister(t *testing.T) {
	es := &EventStream{subs: map[int]*subscription{}}
	_, stop1 := es.Reconnects()
	ch2, _ := es.Reconnects()

	es.mu.Lock()
	n := len(es.recListeners)
	es.mu.Unlock()
	if n != 2 {
		t.Fatalf("listeners = %d, want 2", n)
	}

	stop1()
	es.mu.Lock()
	n = len(es.recListeners)
	es.mu.Unlock()
	if n != 1 {
		t.Errorf("listeners after unregister = %d, want 1", n)
	}

	// The surviving listener still gets pulses.
	es.pulseReconnect()
	select {
	case <-ch2:
	default:
		t.Error("remaining listener did not receive the pulse")
	}
}

// The jitter exists so a single reconnect doesn't stack every in-flight job's
// /jobs backfill onto the master at the same instant.
func TestBackfillJitterIsSpreadAndBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := backfillJitter()
		if d <= 0 || d > reconnectBackfillMax+time.Millisecond {
			t.Fatalf("jitter %v outside (0, %v]", d, reconnectBackfillMax)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Errorf("jitter produced %d distinct values in 200 draws; not spreading", len(seen))
	}
}
