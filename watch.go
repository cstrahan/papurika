package papurika

import (
	"context"
	"errors"
)

// WatchState is the condition Watch blocks for.
type WatchState int

const (
	// WatchAllReturned is satisfied when every targeted minion of a jid returned.
	WatchAllReturned WatchState = iota
	// WatchJobDone is satisfied as soon as a jid has any returns (the master has
	// begun completing it).
	WatchJobDone
)

// Watch blocks until each jid reaches the desired state, returning the current
// normalized Result per jid. It returns IMMEDIATELY for any jid already in the
// target state (an authoritative GET /jobs/<jid> snapshot is taken before any
// blocking, so there is no deadlock), and returns the current snapshot on
// deadline. A missing/aged-out jid is reported with Reason=stream_fatal rather
// than blocking forever.
func (c *Client) Watch(ctx context.Context, jids []string, want WatchState, opts ...RunOption) (map[string]*Result, error) {
	cfg := buildRunConfig(opts)
	if cfg.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.deadline)
		defer cancel()
	}

	out := map[string]*Result{}
	for _, jid := range jids {
		jr, err := c.Job(ctx, jid)
		if errors.Is(err, ErrJobNotFound) {
			out[jid] = &Result{Commands: []CommandResult{{JID: jid, Reason: ReasonStreamFatal}}}
			continue
		}
		if err != nil {
			return out, err
		}
		if watchSatisfied(jr, want) {
			out[jid] = &Result{Commands: []CommandResult{jr.Result}}
			continue
		}
		// Not yet there — gather until satisfied or ctx fires. gather subscribes to
		// events first and reconciles via /jobs, so it converges even across SSE
		// gaps; the upfront snapshot above guarantees we never block on a done job.
		job := Job{JID: jid, Targeted: jr.Meta.Minions, Fun: jr.Meta.Function}
		cr := c.gather(ctx, job, cfg)
		out[jid] = &Result{Commands: []CommandResult{cr}}
		if cr.Reason == ReasonCanceled {
			return out, context.Canceled
		}
		if cr.Reason == ReasonDeadline {
			return out, context.DeadlineExceeded
		}
	}
	return out, nil
}

func watchSatisfied(jr *JobResult, want WatchState) bool {
	switch want {
	case WatchJobDone:
		for _, m := range jr.Result.Minions {
			if m.Status == StatusResponded {
				return true
			}
		}
		return false
	default: // WatchAllReturned
		return jr.Complete
	}
}
