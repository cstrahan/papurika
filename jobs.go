package papurika

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// JobMeta is the metadata salt records for a job.
type JobMeta struct {
	JID       string   `json:"jid"`
	Function  string   `json:"fun"`
	Arguments []any    `json:"arguments,omitempty"`
	Target    string   `json:"target,omitempty"`
	TgtType   string   `json:"target_type,omitempty"`
	User      string   `json:"user,omitempty"`
	StartTime string   `json:"start_time,omitempty"`
	Minions   []string `json:"minions,omitempty"`
}

// JobResult is the normalized view of GET /jobs/<jid>: the metadata plus a
// CommandResult whose Minions include synthesized no-response entries for any
// targeted minion absent from the returns.
type JobResult struct {
	Meta     JobMeta
	Result   CommandResult
	Complete bool
}

// Job fetches and normalizes a single job by jid. A missing/aged-out jid returns
// an error wrapping ErrJobNotFound (detected via salt's explicit Error field).
func (c *Client) Job(ctx context.Context, jid string) (*JobResult, error) {
	res, err := c.authedGet(ctx, "/jobs/"+url.PathEscape(jid))
	if err != nil {
		return nil, err
	}
	return decodeJob(jid, res.body)
}

// Jobs lists known jobs from the cache (GET /jobs), returning metadata by jid.
func (c *Client) Jobs(ctx context.Context) (map[string]JobMeta, error) {
	res, err := c.authedGet(ctx, "/jobs")
	if err != nil {
		return nil, err
	}
	var env struct {
		Return []map[string]struct {
			Function  string     `json:"Function"`
			Arguments []any      `json:"Arguments"`
			Target    flexTarget `json:"Target"`
			TgtType   string     `json:"Target-type"`
			User      string     `json:"User"`
			StartTime string     `json:"StartTime"`
		} `json:"return"`
	}
	if err := json.Unmarshal(res.body, &env); err != nil {
		return nil, apiError(ErrMalformed, http.StatusOK, "application/json", string(res.body), res.module)
	}
	out := map[string]JobMeta{}
	if len(env.Return) > 0 {
		for jid, m := range env.Return[0] {
			out[jid] = JobMeta{
				JID: jid, Function: m.Function, Arguments: m.Arguments,
				Target: m.Target.String(), TgtType: m.TgtType, User: m.User, StartTime: m.StartTime,
			}
		}
	}
	return out, nil
}

// decodeJob normalizes the {info:[meta+Result], return:[{minion:ret}]} shape.
func decodeJob(jid string, body []byte) (*JobResult, error) {
	var env struct {
		Info []struct {
			JID       string          `json:"jid"`
			Function  string          `json:"Function"`
			Arguments []any           `json:"Arguments"`
			Target    flexTarget      `json:"Target"`
			TgtType   string          `json:"Target-type"`
			User      string          `json:"User"`
			StartTime string          `json:"StartTime"`
			Minions   []string        `json:"Minions"`
			Error     string          `json:"Error"`
			Result    json.RawMessage `json:"Result"`
		} `json:"info"`
		Return []json.RawMessage `json:"return"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, apiError(ErrMalformed, http.StatusOK, "application/json", string(body), ModuleUnknown)
	}
	if len(env.Info) == 0 {
		// No info array. On cherrypy this never happens (a missing jid still
		// returns info[0].Error); other modules (e.g. rest_tornado) shape /jobs
		// differently. Treat it as "no data this poll" rather than not-found, so
		// the gather keeps reconciling via the event bus + settle window instead
		// of bailing. (cherrypy-first: we don't parse the tornado shape here.)
		return &JobResult{Result: CommandResult{JID: jid, Minions: map[string]MinionResult{}, raw: body}}, nil
	}
	info := env.Info[0]
	if info.Error != "" {
		return nil, apiError(ErrJobNotFound, http.StatusOK, "application/json", string(body), ModuleUnknown)
	}

	jr := &JobResult{
		Meta: JobMeta{
			JID: pickStr(info.JID, jid), Function: info.Function, Arguments: info.Arguments,
			Target: info.Target.String(), TgtType: info.TgtType, User: info.User,
			StartTime: info.StartTime, Minions: info.Minions,
		},
		Result: CommandResult{
			Fun:      info.Function,
			JID:      pickStr(info.JID, jid),
			Targeted: info.Minions,
			Minions:  map[string]MinionResult{},
			raw:      body,
		},
	}

	// Per-minion returns from info.Result: {minion: {return, retcode, success}}.
	if len(info.Result) > 0 {
		var rm map[string]struct {
			Return  json.RawMessage `json:"return"`
			Retcode *int            `json:"retcode"`
		}
		if json.Unmarshal(info.Result, &rm) == nil {
			for id, v := range rm {
				val := v.Return
				if len(val) == 0 {
					val = json.RawMessage("null")
				}
				jr.Result.Minions[id] = MinionResult{
					ID: id, Status: StatusResponded, Retcode: v.Retcode,
					Value: NewRawValue(val), raw: val,
				}
			}
		}
	}

	// Synthesize no-response for targeted minions absent from the returns.
	for _, id := range info.Minions {
		if _, ok := jr.Result.Minions[id]; !ok {
			jr.Result.Minions[id] = MinionResult{ID: id, Status: StatusNoResponse}
		}
	}

	// Complete when every targeted minion has returned (or there were returns and
	// no outstanding targets).
	jr.Complete = len(jr.Result.Minions) > 0 && len(jr.responders()) == len(info.Minions) && len(info.Minions) > 0
	jr.Result.Complete = jr.Complete
	if jr.Complete {
		jr.Result.Reason = ReasonAllReturned
	}
	return jr, nil
}

func (jr *JobResult) responders() []string {
	var out []string
	for id, m := range jr.Result.Minions {
		if m.Status == StatusResponded {
			out = append(out, id)
		}
	}
	return out
}

func pickStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// flexTarget decodes salt's Target field, which is NOT consistently typed: a
// string for most tgt_types ("*", "master") but a JSON list for
// tgt_type=list (["m1","m2"]). Decoding it as a string fails the whole job,
// and papurika creates list-targeted jobs itself — every saltutil.find_job
// liveness probe is one — so a plain `string` here breaks the job cache
// listing and /jobs/<jid> for anyone who has ever run a job.
//
// It never returns an error: one oddly-shaped field should not take down a
// listing, so an unrecognized shape degrades to its raw text.
type flexTarget struct{ s string }

func (t *flexTarget) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		t.s = one
		return nil
	}
	var many []string
	if json.Unmarshal(b, &many) == nil {
		t.s = strings.Join(many, ",")
		return nil
	}
	t.s = strings.Trim(string(b), `"`)
	return nil
}

func (t flexTarget) String() string { return t.s }

// ActiveJob is one currently-running job, as reported by the jobs.active
// runner. It is the same metadata GET /jobs carries, plus who is still working
// on it.
type ActiveJob struct {
	JobMeta
	// Running maps minion id to the pid executing the job there.
	Running map[string]int `json:"running"`
	// Returned lists minions that have already answered.
	Returned []string `json:"returned,omitempty"`
}

// ActiveJobs lists jobs currently executing somewhere on the fleet, keyed by
// jid (runner jobs.active).
//
// Distinct from Jobs, which lists the master's job *cache* — everything it has
// recorded, finished or not. "What is running right now" is only answerable
// here.
//
// Salt reports the running set as a list of single-key maps,
// [{"m2": 19816}, {"m1": 19815}], which this folds into one map so callers
// never walk that shape.
//
// **This is not a cache read.** The runner fans out a live saltutil.running to
// every minion. It returns as soon as they have all answered — measured at
// ~0.3s against a responsive fleet — but **any minion that does not answer
// costs the master's full `timeout` + `gather_job_timeout`**, 5s + 10s by
// default. Measured: one stopped-but-key-accepted minion takes the call from
// 0.3s to 15.3s. A fleet with dead hosts in it therefore pays ~15s on every
// call, which is most real fleets — the same fact that makes papurika compute
// liveness itself rather than asking.
//
// It runs the runner **asynchronously** and gathers the return off the event
// bus, so that ~15s is spent waiting on an event rather than holding a
// cherrypy worker thread hostage — see ExecAsync. A deadline on ctx bounds the
// wait and still yields the jid.
func (c *Client) ActiveJobs(ctx context.Context) (map[string]ActiveJob, error) {
	cmd := Runner("jobs.active")
	cmd.Client = ClientRunnerAsync
	res, err := c.ExecAsync(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if len(res.Commands) == 0 {
		return map[string]ActiveJob{}, nil
	}
	raw := res.Commands[0].Value.Raw()
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]ActiveJob{}, nil
	}

	var wire map[string]struct {
		Function  string           `json:"Function"`
		Arguments []any            `json:"Arguments"`
		Target    flexTarget       `json:"Target"`
		TgtType   string           `json:"Target-type"`
		User      string           `json:"User"`
		StartTime string           `json:"StartTime"`
		Running   []map[string]int `json:"Running"`
		Returned  []string         `json:"Returned"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, apiError(ErrMalformed, http.StatusOK, "application/json", string(raw), ModuleUnknown)
	}

	out := make(map[string]ActiveJob, len(wire))
	for jid, w := range wire {
		running := map[string]int{}
		for _, entry := range w.Running {
			for minion, pid := range entry {
				running[minion] = pid
			}
		}
		out[jid] = ActiveJob{
			JobMeta: JobMeta{
				JID: jid, Function: w.Function, Arguments: w.Arguments,
				Target: w.Target.String(), TgtType: w.TgtType, User: w.User,
				StartTime: w.StartTime,
			},
			Running:  running,
			Returned: w.Returned,
		}
	}
	return out, nil
}
