package papurika

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

// batchSpyServer records every lowstate POSTed to /, answering local_async
// submits with a fresh jid and the minions the chunk named.
type batchSpyServer struct {
	*httptest.Server
	mu    sync.Mutex
	posts []map[string]any
	jobs  map[string][]string // jid -> minions it targeted
	n     int
}

// newBatchSpy answers login, local_async submits (with a fresh jid echoing the
// chunk's minions) and /jobs/<jid> polls reporting every one of them returned,
// so a gather completes promptly. /events 404s, so the engine falls back to
// polling — which keeps the test free of SSE timing.
func newBatchSpy(t *testing.T, allMinions []string) *batchSpyServer {
	t.Helper()
	s := &batchSpyServer{jobs: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "CherryPy/18")
		switch {
		case r.URL.Path == "/login":
			io.WriteString(w, loginBody("TOK1"))

		case strings.HasPrefix(r.URL.Path, "/jobs/"):
			jid := strings.TrimPrefix(r.URL.Path, "/jobs/")
			s.mu.Lock()
			mins := s.jobs[jid]
			s.mu.Unlock()
			var b strings.Builder
			b.WriteString(`{"info":[{"jid":"` + jid + `","Function":"test.ping","Target":"*","Minions":`)
			j, _ := json.Marshal(mins)
			b.Write(j)
			b.WriteString(`,"Result":{`)
			for i, m := range mins {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`"` + m + `":{"return":true,"retcode":0,"success":true}`)
			}
			b.WriteString(`}}],"return":[{}]}`)
			io.WriteString(w, b.String())

		case r.URL.Path == "/":
			body, _ := io.ReadAll(r.Body)
			var low []map[string]any
			_ = json.Unmarshal(body, &low)

			mins := allMinions
			if tgt, ok := low[0]["tgt"].(string); ok && low[0]["tgt_type"] == "list" {
				mins = strings.Split(tgt, ",")
			}
			s.mu.Lock()
			s.posts = append(s.posts, low...)
			s.n++
			jid := fmt.Sprintf("2026010101010101%04d", s.n)
			s.jobs[jid] = mins
			s.mu.Unlock()

			j, _ := json.Marshal(mins)
			io.WriteString(w, `{"return":[{"jid":"`+jid+`","minions":`+string(j)+`}]}`)

		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *batchSpyServer) lowstates() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.posts...)
}

func batchClient(t *testing.T, srv *httptest.Server) *Client {
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

// Submit used to drop JobSpec.Batch on the floor and publish ONE job to the
// entire target — the opposite of limiting the blast radius, and silent. It
// must chunk.
func TestSubmitBatchesClientSide(t *testing.T) {
	fastPolls(t)
	all := []string{"m1", "m2", "m3", "m4"}
	srv := newBatchSpy(t, all)
	c := batchClient(t, srv.Server)

	sub, err := c.Submit(context.Background(),
		JobSpec{Target: "*", Fun: "test.ping", Batch: "2"})
	if err != nil {
		t.Fatal(err)
	}

	// resolveTargets fires one glob-targeted discovery ping first; the chunks
	// are the list-targeted submits after it.
	var chunks []map[string]any
	for _, l := range srv.lowstates() {
		if l["client"] == "local_async" && l["tgt_type"] == "list" {
			chunks = append(chunks, l)
		}
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d async submits, want 2 chunks of 2 (lowstates: %+v)", len(chunks), srv.lowstates())
	}
	if len(sub.JIDs()) != 2 {
		t.Errorf("JIDs() = %v, want one per chunk", sub.JIDs())
	}
	for _, l := range chunks {
		if l["tgt_type"] != "list" {
			t.Errorf("chunk not list-targeted: %+v", l)
		}
		if got := strings.Count(l["tgt"].(string), ",") + 1; got != 2 {
			t.Errorf("chunk targeted %d minions, want 2: %v", got, l["tgt"])
		}
		// The per-chunk lowstate must NOT carry batch, or salt would be asked
		// to batch an already-chunked list.
		if _, ok := l["batch"]; ok {
			t.Errorf("per-chunk lowstate still carries batch: %+v", l)
		}
	}
}

// Server-side batching cannot produce a jid, so Submit must refuse rather than
// quietly doing something else.
func TestSubmitRejectsServerBatch(t *testing.T) {
	fastPolls(t)
	srv := newBatchSpy(t, []string{"m1", "m2"})
	c := batchClient(t, srv.Server)

	_, err := c.Submit(context.Background(),
		JobSpec{Target: "*", Fun: "test.ping", Batch: "1"}, WithServerBatch())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	for _, l := range srv.lowstates() {
		if l["client"] == "local_async" || l["client"] == "local_batch" {
			t.Errorf("nothing should have been published, got %+v", l)
		}
	}
}

// Run's default is client-side too, so a batched run keeps its jids.
func TestRunBatchDefaultsToClientSide(t *testing.T) {
	srv := newBatchSpy(t, []string{"m1", "m2", "m3"})
	c := batchClient(t, srv.Server)
	fastPolls(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // don't gather; we only care which client was published to
	_, _ = c.Run(ctx, JobSpec{Target: "*", Fun: "test.ping", Batch: "1"})

	for _, l := range srv.lowstates() {
		if l["client"] == "local_batch" {
			t.Fatalf("Run defaulted to server-side local_batch: %+v", l)
		}
	}
}

// WithServerBatch still reaches salt's local_batch, with the size intact.
func TestRunServerBatchUsesLocalBatch(t *testing.T) {
	fastPolls(t)
	srv := newBatchSpy(t, []string{"m1", "m2"})
	c := batchClient(t, srv.Server)

	_, _ = c.Run(context.Background(),
		JobSpec{Target: "*", Fun: "test.ping", Batch: "2"}, WithServerBatch())

	var found bool
	for _, l := range srv.lowstates() {
		if l["client"] == "local_batch" {
			found = true
			if l["batch"] != "2" {
				t.Errorf("batch = %v, want \"2\"", l["batch"])
			}
		}
	}
	if !found {
		t.Errorf("WithServerBatch did not use local_batch: %+v", srv.lowstates())
	}
}

// A client-side-batched run is several jobs behind one merged result. Keeping
// only the first chunk's jid handed an interrupted run one recoverable job out
// of N, which is exactly what the engine promises never to do.
func TestRunClientBatchKeepsEveryJID(t *testing.T) {
	fastPolls(t)
	srv := newBatchSpy(t, []string{"m1", "m2", "m3", "m4"})
	c := batchClient(t, srv.Server)

	res, err := c.Run(context.Background(),
		JobSpec{Target: "*", Fun: "test.ping", Batch: "1"})
	if err != nil {
		t.Fatal(err)
	}

	// Four minions, batch of 1 => four chunk jobs, four jids.
	if got := res.JIDs(); len(got) != 4 {
		t.Fatalf("JIDs() = %v (%d), want one per chunk (4)", got, len(got))
	}
	cr := res.Commands[0]
	if cr.JID == "" || len(cr.ExtraJIDs) != 3 {
		t.Errorf("JID=%q ExtraJIDs=%v, want a primary plus 3 extras", cr.JID, cr.ExtraJIDs)
	}
	// Ordering must not depend on which chunk finished first.
	if !sort.StringsAreSorted(append([]string{cr.JID}, cr.ExtraJIDs...)) {
		t.Errorf("chunk jids are not in deterministic order: %q %v", cr.JID, cr.ExtraJIDs)
	}
	// And the merged view still unifies the minions.
	if len(cr.Minions) != 4 {
		t.Errorf("merged minions = %d, want 4", len(cr.Minions))
	}
}
