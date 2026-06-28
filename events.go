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

	mu         sync.Mutex
	subs       map[int]*subscription
	nextID     int
	reconnects chan struct{}
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
		c:          c,
		ctx:        sctx,
		cancel:     cancel,
		subs:       map[int]*subscription{},
		reconnects: make(chan struct{}, 1),
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

// Reconnects pulses (non-blocking) each time the underlying connection is
// re-established after a drop.
func (es *EventStream) Reconnects() <-chan struct{} { return es.reconnects }

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
	select {
	case es.reconnects <- struct{}{}:
	default:
	}
}
