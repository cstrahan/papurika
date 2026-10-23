package papurika

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExecAsync runs a master-side command (runner or wheel) asynchronously and
// gathers its return, instead of holding a synchronous connection open for the
// duration.
//
// This matters for any runner that touches the fleet. A sync POST pins a
// cherrypy worker thread until the runner finishes, and jobs.active — which
// fans out a live saltutil.running to every minion — takes the master's full
// timeout + gather_job_timeout (~15s) whenever a single minion fails to answer.
// Holding a worker that long is exactly what papurika's async-first rule
// exists to prevent (design/async.md).
//
// The returned *Result has the same shape sync Exec produces for the same
// command, so callers and renderers do not branch on how it was obtained. On
// ctx cancellation the partial Result still carries the jid, so the work stays
// recoverable.
func (c *Client) ExecAsync(ctx context.Context, cmd Command, opts ...RunOption) (*Result, error) {
	prefix, err := masterEventPrefix(cmd.Client)
	if err != nil {
		return nil, err
	}
	cfg := buildRunConfig(opts)
	if cfg.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.deadline)
		defer cancel()
	}

	// Subscribe before submitting, or a fast runner can return before we are
	// listening. We cannot filter on the jid yet — we only learn it from the
	// submit — so we take the whole salt/run/ (or salt/wheel/) prefix and
	// match the jid as events arrive.
	var events <-chan Event
	if stream, release := c.acquireEvents(ctx); stream != nil {
		defer release()
		var unsub func()
		events, unsub = stream.Subscribe(prefix)
		defer unsub()
	}

	jid, err := c.submitMaster(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if cfg.onSubmit != nil {
		cfg.onSubmit(Job{JID: jid, Fun: cmd.Fun, Client: cmd.Client})
	}
	return c.gatherMaster(ctx, cmd, jid, prefix, events)
}

// masterEventPrefix maps an async master client to the event tag prefix its
// job reports under. Local jobs use salt/job/; the master-side clients do not.
func masterEventPrefix(ct ClientType) (string, error) {
	switch ct {
	case ClientRunnerAsync:
		return "salt/run/", nil
	case ClientWheelAsync:
		return "salt/wheel/", nil
	default:
		return "", fmt.Errorf("%w: ExecAsync needs an async master client, got %q", ErrUnsupported, ct)
	}
}

// submitMaster POSTs the async master command and returns its jid. The
// response is {"tag": "salt/run/<jid>", "jid": "<jid>"}.
func (c *Client) submitMaster(ctx context.Context, cmd Command) (string, error) {
	res, err := c.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	if len(res.Commands) == 0 {
		return "", apiError(ErrMalformed, 0, "", "", ModuleUnknown)
	}
	cr := res.Commands[0]
	if cr.JID != "" {
		return cr.JID, nil
	}
	// The envelope decoder keys off local-job shapes; fall back to reading the
	// {tag, jid} pair straight out of the value.
	var sub struct {
		JID string `json:"jid"`
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(cr.Value.Raw(), &sub); err == nil && sub.JID != "" {
		return sub.JID, nil
	}
	return "", apiError(ErrMalformed, 0, "", string(cr.Raw()), ModuleUnknown)
}

// gatherMaster waits for the job's return, from the event bus when it arrives
// and from the job cache otherwise.
//
// Both paths are needed for the same reason local gathers need both: the event
// stream is lossy (no replay across a reconnect gap), so a missed /ret would
// otherwise hang until the deadline. A runner jid IS resolvable through
// /jobs/<jid> — verified, see cherrypy_learnings.md — which makes the poll a
// real backstop rather than a hope.
func (c *Client) gatherMaster(ctx context.Context, cmd Command, jid, prefix string, events <-chan Event) (*Result, error) {
	retTag := prefix + jid + "/ret"
	partial := func(reason CompletionReason) *Result {
		return &Result{Commands: []CommandResult{{
			Client: cmd.Client, Fun: cmd.Fun, JID: jid,
			Minions: map[string]MinionResult{}, Reason: reason,
		}}}
	}

	poll := time.NewTimer(initialPollDelay)
	defer poll.Stop()
	interval := initialPollDelay

	for {
		select {
		case <-ctx.Done():
			// The jid survives on the partial result; the job keeps running.
			r := partial(ReasonDeadline)
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.Commands[0].Reason = ReasonCanceled
			}
			return r, ctx.Err()

		case ev, ok := <-events:
			if !ok {
				events = nil // stream closed; polling carries on
				continue
			}
			if ev.Tag != retTag {
				continue
			}
			return masterResult(cmd, jid, ev.Data), nil

		case <-poll.C:
			if r, done := c.pollMaster(ctx, cmd, jid); done {
				return r, nil
			}
			if interval < pollBackoffMax {
				interval *= 2
				if interval > pollBackoffMax {
					interval = pollBackoffMax
				}
			}
			poll.Reset(interval)
		}
	}
}

// pollMaster checks the job cache for a finished master job. The runner's
// envelope sits at return[0].master for this shape.
//
// Any error — ErrJobNotFound included — means "keep waiting", which is the
// opposite of the local gather, where a missing jid is fatal. A runner is
// written to the job cache only when it COMPLETES (verified: a 15s runner
// 404s for its whole execution), so not-found is the normal state of a runner
// that is still going, not evidence that it aged out.
func (c *Client) pollMaster(ctx context.Context, cmd Command, jid string) (*Result, bool) {
	jr, err := c.Job(ctx, jid)
	if err != nil || jr == nil {
		return nil, false // not recorded yet, or a transient failure; keep waiting
	}
	m, ok := jr.Result.Minions["master"]
	if !ok || m.Value.IsZero() {
		return nil, false
	}
	return masterResult(cmd, jid, m.Value.Raw()), true
}

// masterResult decodes a runner/wheel return envelope
// ({fun, jid, user, return, success}) into the same Result shape sync Exec
// produces, so nothing downstream has to know which path produced it.
func masterResult(cmd Command, jid string, envelope json.RawMessage) *Result {
	var env struct {
		Return  json.RawMessage `json:"return"`
		Success *bool           `json:"success"`
	}
	value := envelope
	var success *bool
	if json.Unmarshal(envelope, &env) == nil && len(env.Return) > 0 {
		value = env.Return
		success = env.Success
	}
	cr := CommandResult{
		Client:   cmd.Client,
		Fun:      cmd.Fun,
		JID:      jid,
		Value:    NewRawValue(value),
		Minions:  map[string]MinionResult{},
		Complete: true,
		Reason:   ReasonAllReturned,
		raw:      envelope,
	}
	if success != nil {
		cr.Success = success
	}
	return &Result{Commands: []CommandResult{cr}}
}

// RunnerFailed reports whether a runner return looks like salt's
// argument/usage error rather than a result.
//
// It exists because salt's own success flag cannot be used for this: a runner
// invoked with missing arguments still returns success:true, with the usage
// text as its return VALUE (verified — see cherrypy_learnings.md). The flag
// means "the runner executed", not "the runner worked", so anything keying off
// it alone reports a broken call as a successful one.
//
// This is a heuristic on an English string and is deliberately narrow: it
// catches the invalid-argument case, which is the one a client can act on.
func RunnerFailed(v RawValue) (string, bool) {
	s, ok := v.String()
	if !ok {
		return "", false
	}
	if strings.Contains(s, "Passed invalid arguments:") {
		return strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]), true
	}
	return "", false
}
