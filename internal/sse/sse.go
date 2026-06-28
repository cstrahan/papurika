// Package sse reads a salt-api /events Server-Sent Events stream. salt frames
// carry a non-standard "tag:" field alongside the standard "data:" JSON, letting
// clients filter by tag without deserializing. The stream is lossy (no replay
// across reconnect gaps, no idle heartbeat, dies on master restart), so callers
// must pair it with /jobs reconciliation.
package sse

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Frame is one SSE event: the salt tag and the raw JSON from the data field
// (the {"tag":..., "data":{...}} object).
type Frame struct {
	Tag  string
	Data []byte
}

// Conn is an open /events stream.
type Conn struct {
	resp *http.Response
	sc   *bufio.Scanner
}

// Open issues GET <eventsURL> and returns a Conn. It deliberately does NOT send
// Accept: text/event-stream (rest_tornado rejects that header with 406; cherrypy
// is happy without it). The token is sent as the X-Auth-Token header.
func Open(ctx context.Context, httpc *http.Client, eventsURL, token string) (*Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("sse: GET %s: HTTP %d: %s", eventsURL, resp.StatusCode, body)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow large event payloads
	return &Conn{resp: resp, sc: sc}, nil
}

// Next reads the next complete frame. It blocks until a blank-line-terminated
// event arrives, the stream ends (io.EOF), or the context bound to Open fires
// (surfacing as a read error). Frames with no tag and no data are skipped.
func (c *Conn) Next() (Frame, error) {
	var tag string
	var data []string
	for c.sc.Scan() {
		line := c.sc.Text()
		if line == "" { // end of event
			if tag == "" && len(data) == 0 {
				continue // keepalive / stray blank
			}
			return Frame{Tag: tag, Data: []byte(strings.Join(data, "\n"))}, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "tag":
			tag = value
		case "data":
			data = append(data, value)
		}
		// "retry", "id", and unknown fields are ignored.
	}
	if err := c.sc.Err(); err != nil {
		return Frame{}, err
	}
	return Frame{}, io.EOF
}

// Close terminates the stream.
func (c *Conn) Close() error {
	if c.resp != nil && c.resp.Body != nil {
		return c.resp.Body.Close()
	}
	return nil
}
