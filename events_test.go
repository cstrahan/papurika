package papurika

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cstrahan/papurika/internal/sse"
)

// newTestStream builds an EventStream without an SSE connection, so tests can
// drive broadcast()/pulseReconnect() directly (white-box).
func newTestStream() *EventStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &EventStream{ctx: ctx, cancel: cancel, subs: map[int]*subscription{}}
}

func frame(tag string, data string) sse.Frame {
	return sse.Frame{Tag: tag, Data: []byte(`{"tag":"` + tag + `","data":` + data + `}`)}
}

func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func expectNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func recvPulse(t *testing.T, ch <-chan struct{}, who string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("%s did not receive the reconnect pulse", who)
	}
}

func TestEventStreamFanoutAndFilter(t *testing.T) {
	es := newTestStream()
	defer es.Close()

	all, _ := es.Subscribe("")                    // every tag
	job1, _ := es.Subscribe("salt/job/1/")        // only job 1
	job1ret, _ := es.Subscribe("salt/job/1/ret/") // only job 1 returns

	// A ret for job 1 — all three should see it.
	es.broadcast(frame("salt/job/1/ret/m1", `{"return":true,"retcode":0}`))
	for name, ch := range map[string]<-chan Event{"all": all, "job1": job1, "job1ret": job1ret} {
		ev := recvEvent(t, ch)
		if ev.Tag != "salt/job/1/ret/m1" {
			t.Errorf("%s: tag = %q", name, ev.Tag)
		}
		// Data must be the inner payload (the "data" object), not the wrapper.
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil || d["return"] != true {
			t.Errorf("%s: data = %s (want inner {return:true,…})", name, ev.Data)
		}
	}

	// A "new" event for job 2 — only the catch-all subscriber matches.
	es.broadcast(frame("salt/job/2/new", `{"minions":["m1"]}`))
	if ev := recvEvent(t, all); ev.Tag != "salt/job/2/new" {
		t.Errorf("all: tag = %q", ev.Tag)
	}
	expectNoEvent(t, job1)    // prefix "salt/job/1/" must not match job 2
	expectNoEvent(t, job1ret) // ditto
}

func TestEventStreamUnsubscribe(t *testing.T) {
	es := newTestStream()
	defer es.Close()

	ch, unsub := es.Subscribe("")
	es.broadcast(frame("salt/job/1/ret/m1", `true`))
	recvEvent(t, ch) // delivered before unsubscribe

	unsub()
	es.broadcast(frame("salt/job/1/ret/m2", `true`))
	expectNoEvent(t, ch) // no more deliveries after unsubscribe

	es.mu.Lock()
	n := len(es.subs)
	es.mu.Unlock()
	if n != 0 {
		t.Errorf("subs not cleaned up: %d remain", n)
	}
}

// TestEventStreamReconnectAllListeners is the crux: every consumer that called
// Reconnects() must see each pulse (a reconnect drops events for all in-flight
// jobs, so each gather must back-fill).
func TestEventStreamReconnectAllListeners(t *testing.T) {
	es := newTestStream()
	defer es.Close()

	r1, _ := es.Reconnects()
	r2, _ := es.Reconnects()
	r3, _ := es.Reconnects()

	es.pulseReconnect()

	recvPulse(t, r1, "listener 1")
	recvPulse(t, r2, "listener 2")
	recvPulse(t, r3, "listener 3")
}

func TestEventStreamReconnectPulseCoalesces(t *testing.T) {
	es := newTestStream()
	defer es.Close()
	r, _ := es.Reconnects()

	// Multiple pulses with no intervening read collapse to (at least) one — the
	// listener buffer is 1, and a backfill poll only needs to happen once.
	es.pulseReconnect()
	es.pulseReconnect()
	es.pulseReconnect()

	recvPulse(t, r, "listener")
	// No second pulse buffered.
	select {
	case <-r:
		t.Fatal("expected pulses to coalesce to one")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventStreamConcurrentSubscribers(t *testing.T) {
	es := newTestStream()
	defer es.Close()

	const n = 16
	chans := make([]<-chan Event, n)
	for i := range chans {
		chans[i], _ = es.Subscribe("")
	}

	var wg sync.WaitGroup
	got := make([]int, n)
	for i := range chans {
		wg.Add(1)
		go func(i int, ch <-chan Event) {
			defer wg.Done()
			for {
				select {
				case <-ch:
					got[i]++
				case <-time.After(200 * time.Millisecond):
					return
				}
			}
		}(i, chans[i])
	}

	const events = 10
	for j := 0; j < events; j++ {
		es.broadcast(frame("salt/job/1/ret/m1", `true`))
	}
	wg.Wait()

	for i, c := range got {
		if c != events {
			t.Errorf("subscriber %d got %d events, want %d", i, c, events)
		}
	}
}
