package papurika

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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
			Function  string `json:"Function"`
			Arguments []any  `json:"Arguments"`
			Target    string `json:"Target"`
			TgtType   string `json:"Target-type"`
			User      string `json:"User"`
			StartTime string `json:"StartTime"`
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
				Target: m.Target, TgtType: m.TgtType, User: m.User, StartTime: m.StartTime,
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
			Target    string          `json:"Target"`
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
		return nil, apiError(ErrJobNotFound, http.StatusOK, "application/json", string(body), ModuleUnknown)
	}
	info := env.Info[0]
	if info.Error != "" {
		return nil, apiError(ErrJobNotFound, http.StatusOK, "application/json", string(body), ModuleUnknown)
	}

	jr := &JobResult{
		Meta: JobMeta{
			JID: pickStr(info.JID, jid), Function: info.Function, Arguments: info.Arguments,
			Target: info.Target, TgtType: info.TgtType, User: info.User,
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
