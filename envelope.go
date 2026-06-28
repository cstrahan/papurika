package papurika

import (
	"encoding/json"
	"strings"
)

// decodeExecResponse normalizes a POST / (or /run) response body into a Result,
// pairing each sent command with its return entry. salt-api returns
// {"return": [<entry per command>]}, except local_batch, which fans a single
// command's results across one entry PER CHUNK. So:
//
//   - one command  -> all return entries belong to it (merged; handles batch)
//   - N commands   -> pair positionally (batch is unsupported when mixed)
func decodeExecResponse(cmds []Command, body []byte) (*Result, error) {
	var env struct {
		Return json.RawMessage `json:"return"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, apiError(ErrMalformed, 0, "application/json", string(body), ModuleUnknown)
	}
	var entries []json.RawMessage
	if len(env.Return) > 0 {
		// Tolerate a non-array (shouldn't happen for Exec, but be defensive).
		if err := json.Unmarshal(env.Return, &entries); err != nil {
			entries = []json.RawMessage{env.Return}
		}
	}

	res := &Result{}
	switch {
	case len(cmds) == 1:
		res.Commands = []CommandResult{decodeCommand(cmds[0], entries)}
	default:
		for i, cmd := range cmds {
			var sub []json.RawMessage
			if i < len(entries) {
				sub = entries[i : i+1]
			}
			res.Commands = append(res.Commands, decodeCommand(cmd, sub))
		}
	}
	return res, nil
}

// decodeCommand decodes one command's return entries (≥1 for batch fan-out).
func decodeCommand(cmd Command, entries []json.RawMessage) CommandResult {
	cr := CommandResult{Client: cmd.Client, Fun: cmd.Fun}
	if len(entries) == 1 {
		cr.raw = entries[0]
	} else if len(entries) > 1 {
		cr.raw, _ = json.Marshal(entries)
	}

	switch {
	case cmd.Client.IsAsync():
		if len(entries) > 0 {
			decodeAsync(entries[0], &cr)
		}
	case cmd.Client == ClientRunner || cmd.Client == ClientWheel:
		if len(entries) > 0 {
			decodeRunnerWheel(entries[0], &cr)
		}
	default: // local, local_batch, local_subset, ssh
		cr.Minions = decodeMinions(entries)
	}
	return cr
}

// decodeMinions merges per-minion outcomes across one-or-more return entries
// (local_batch fans out one entry per chunk; ids are unique keys, so the merge is
// lossless — see pepper_design_analysis.md §2.12).
func decodeMinions(entries []json.RawMessage) map[string]MinionResult {
	out := map[string]MinionResult{}
	for _, e := range entries {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e, &m); err != nil {
			continue // not a {minion: ...} object (e.g. empty / unexpected)
		}
		for id, val := range m {
			out[id] = decodeMinionValue(id, val)
		}
	}
	return out
}

// decodeMinionValue classifies a single minion's return value. It folds salt's
// three "no response" surfacings into StatusNoResponse and detects the
// unknown-function string, while always preserving the raw bytes.
func decodeMinionValue(id string, raw json.RawMessage) MinionResult {
	mr := MinionResult{ID: id, raw: raw, Value: NewRawValue(raw)}

	// String returns: cmd.run output, or salt's English status strings.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch {
		case strings.Contains(s, "Minion did not return"):
			mr.Status = StatusNoResponse
		case strings.Contains(s, "is not available"):
			mr.Status = StatusError
		default:
			mr.Status = StatusResponded
		}
		return mr
	}

	// Object returns: full_return wrapper {ret, retcode, jid}, batch splice
	// {retcode, <payload>}, or a bare structured return.
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		mr.Status = StatusResponded
		if ret, ok := obj["ret"]; ok { // full_return wrapper (non-batch local)
			mr.Value = NewRawValue(ret)
		}
		if rc, ok := obj["retcode"]; ok {
			if n, ok := jsonInt(rc); ok {
				mr.Retcode = intPtr(n)
			}
		}
		return mr
	}

	// Scalar (bool/number) or null: e.g. test.ping -> true.
	mr.Status = StatusResponded
	return mr
}

// decodeAsync extracts {jid, minions} (local_async) or {tag, jid}
// (runner_async/wheel_async).
func decodeAsync(entry json.RawMessage, cr *CommandResult) {
	var a struct {
		JID     string   `json:"jid"`
		Minions []string `json:"minions"`
	}
	_ = json.Unmarshal(entry, &a)
	cr.JID = a.JID
	cr.Targeted = a.Minions
}

// decodeRunnerWheel unwraps the full_return / wheel envelope
// {tag, data:{return, success, retcode}}; falls back to a bare value.
func decodeRunnerWheel(entry json.RawMessage, cr *CommandResult) {
	var w struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(entry, &w) == nil && len(w.Data) > 0 {
		var d struct {
			Return  json.RawMessage `json:"return"`
			Success *bool           `json:"success"`
			Retcode json.RawMessage `json:"retcode"`
		}
		if json.Unmarshal(w.Data, &d) == nil && (len(d.Return) > 0 || d.Success != nil) {
			cr.Value = NewRawValue(d.Return)
			cr.Success = d.Success
			if n, ok := jsonInt(d.Retcode); ok {
				cr.Retcode = intPtr(n)
			}
			return
		}
	}
	// Bare value (no full_return wrapper).
	cr.Value = NewRawValue(entry)
}

// jsonInt parses a JSON number into an int.
func jsonInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}
