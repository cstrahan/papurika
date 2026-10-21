package papurika

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/cstrahan/papurika/internal/sse"
)

// Event is a salt event-bus message: the tag and the inner data payload (the
// "data" object of the SSE frame's JSON).
type Event struct {
	Tag  string
	Data json.RawMessage
}

// EventStream multiplexes a single /events SSE connection to many subscribers,
// filtered by tag prefix, and auto-reconnects. Because the stream is lossy (no
// replay across a reconnect gap, no idle heartbeat, dies on master restart),
// each reconnect pulses Reconnects() so consumers can force a /jobs backfill.
type EventStream struct {
	c      *Client
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	subs         map[int]*subscription
	nextID       int
	recListeners map[int]chan struct{} // one per Reconnects() caller; each gets every pulse
	nextRecID    int
}

type subscription struct {
	prefix string
	ch     chan Event
}

// Events opens the shared event stream. The returned stream stays alive until
// Close or until ctx is canceled.
func (c *Client) Events(ctx context.Context) (*EventStream, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	// Surface an immediate connection error to the caller before going async.
	conn, err := sse.Open(sctx, c.httpc, c.base+"/events", tok)
	if err != nil {
		cancel()
		return nil, apiError(ErrTransport, 0, "", err.Error(), ModuleUnknown)
	}
	es := &EventStream{
		c:            c,
		ctx:          sctx,
		cancel:       cancel,
		subs:         map[int]*subscription{},
		recListeners: map[int]chan struct{}{},
	}
	go es.run(conn)
	return es, nil
}

// Subscribe registers interest in events whose tag starts with tagPrefix. It
// returns a buffered channel and an unsubscribe func. Events are dropped if the
// channel is full — completion is reconciled against /jobs, so a dropped event
// is not fatal.
func (es *EventStream) Subscribe(tagPrefix string) (<-chan Event, func()) {
	es.mu.Lock()
	defer es.mu.Unlock()
	id := es.nextID
	es.nextID++
	sub := &subscription{prefix: tagPrefix, ch: make(chan Event, 256)}
	es.subs[id] = sub
	return sub.ch, func() {
		es.mu.Lock()
		delete(es.subs, id)
		es.mu.Unlock()
	}
}

// Reconnects returns a channel that pulses (non-blocking) each time the
// underlying connection is re-established after a drop, plus a func that
// unregisters it. Each call registers an independent listener, so every
// consumer of a shared stream sees every pulse — important because a reconnect
// can drop events for all in-flight jobs, and each must force its own /jobs
// backfill.
//
// Call the unregister func when done. A stream shared across many short-lived
// jobs outlives all of them, so a listener that is never removed is a leak that
// also slows every subsequent pulse.
func (es *EventStream) Reconnects() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	es.mu.Lock()
	if es.recListeners == nil { // a zero-value EventStream is usable
		es.recListeners = map[int]chan struct{}{}
	}
	id := es.nextRecID
	es.nextRecID++
	es.recListeners[id] = ch
	es.mu.Unlock()
	return ch, func() {
		es.mu.Lock()
		delete(es.recListeners, id)
		es.mu.Unlock()
	}
}

// Close terminates the stream and all subscriptions.
func (es *EventStream) Close() { es.cancel() }

func (es *EventStream) run(conn *sse.Conn) {
	backoff := 200 * time.Millisecond
	for {
		for {
			f, err := conn.Next()
			if err != nil {
				conn.Close()
				break
			}
			backoff = 200 * time.Millisecond // healthy read resets backoff
			es.broadcast(f)
		}
		if es.ctx.Err() != nil {
			return
		}
		// Reconnect with backoff; refresh the token in case it expired.
		select {
		case <-es.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
		tok, err := es.c.ensureToken(es.ctx)
		if err != nil {
			continue
		}
		nc, err := sse.Open(es.ctx, es.c.httpc, es.c.base+"/events", tok)
		if err != nil {
			continue
		}
		conn = nc
		es.pulseReconnect()
	}
}

func (es *EventStream) broadcast(f sse.Frame) {
	tag := f.Tag
	var data json.RawMessage
	// The data line is {"tag":..., "data":{...}}; prefer the inner data object.
	var wrapper struct {
		Tag  string          `json:"tag"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(f.Data, &wrapper) == nil {
		if tag == "" {
			tag = wrapper.Tag
		}
		data = wrapper.Data
	} else {
		data = f.Data
	}
	ev := Event{Tag: tag, Data: data}

	es.mu.Lock()
	defer es.mu.Unlock()
	for _, sub := range es.subs {
		if sub.prefix == "" || strings.HasPrefix(tag, sub.prefix) {
			select {
			case sub.ch <- ev:
			default: // drop on overflow; /jobs reconciles
			}
		}
	}
}

func (es *EventStream) pulseReconnect() {
	es.mu.Lock()
	defer es.mu.Unlock()
	for _, ch := range es.recListeners {
		select {
		case ch <- struct{}{}:
		default: // listener already has a pending pulse; one is enough
		}
	}
}

// acquireEvents returns the Client's shared event stream, opening it on first
// use, together with a release func the caller must call when done.
//
// There is one connection per Client rather than one per job on purpose. Each
// /events connection occupies a cherrypy worker thread for as long as it is
// held, so a 50-host deploy that opened one per in-flight job would hold 50
// workers hostage on a server whose worker pool is the scarce resource — while
// every one of those connections carries the exact same firehose, since salt
// does no server-side filtering. EventStream already fans one connection out to
// many tag-filtered subscribers, so the connection is the only thing worth
// sharing.
//
// The stream is refcounted rather than tied to a Client lifetime: it exists
// only while jobs are in flight, so an idle Client holds no connection and no
// Close method has to be invented. It is deliberately NOT tied to the caller's
// context — the gather that happens to open it must not take it down when it
// finishes while other gathers are still using it.
//
// Returns (nil, no-op) if the stream cannot be opened; callers fall back to
// polling.
func (c *Client) acquireEvents(ctx context.Context) (*EventStream, func()) {
	c.evMu.Lock()
	defer c.evMu.Unlock()

	if c.evStream != nil && c.evStream.ctx.Err() == nil {
		c.evRefs++
		return c.evStream, c.releaseEvents
	}
	// Either first use, or the previous stream died for good; open a new one.
	stream, err := c.Events(context.WithoutCancel(ctx))
	if err != nil {
		c.log.Debug("event stream unavailable; falling back to polling", "err", err)
		return nil, func() {}
	}
	c.evStream = stream
	c.evRefs = 1
	return stream, c.releaseEvents
}

// releaseEvents drops one reference, closing the shared stream when the last
// job using it is done.
func (c *Client) releaseEvents() {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	c.evRefs--
	if c.evRefs > 0 {
		return
	}
	if c.evStream != nil {
		c.evStream.Close()
		c.evStream = nil
	}
	c.evRefs = 0
}
