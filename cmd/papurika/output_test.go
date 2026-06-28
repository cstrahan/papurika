package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cstrahan/papurika"
)

func intp(n int) *int { return &n }

// res builds a one-command local Result with the given minion statuses.
func res(minions map[string]papurika.MinionResult, targeted ...string) *papurika.Result {
	return &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocal, Fun: "test.ping", JID: "1",
		Targeted: targeted, Minions: minions,
	}}}
}

func mr(id string, st papurika.Status, rc *int) papurika.MinionResult {
	return papurika.MinionResult{ID: id, Status: st, Retcode: rc, Value: papurika.NewRawValue(json.RawMessage("true"))}
}

func TestExitCodePolicies(t *testing.T) {
	allOK := res(map[string]papurika.MinionResult{
		"a": mr("a", papurika.StatusResponded, intp(0)),
		"b": mr("b", papurika.StatusResponded, intp(0)),
	}, "a", "b")
	partial := res(map[string]papurika.MinionResult{
		"a": mr("a", papurika.StatusResponded, intp(0)),
		"b": mr("b", papurika.StatusResponded, intp(3)),
	}, "a", "b")
	allFail := res(map[string]papurika.MinionResult{
		"a": mr("a", papurika.StatusResponded, intp(1)),
		"b": mr("b", papurika.StatusResponded, intp(2)),
	}, "a", "b")
	withNR := res(map[string]papurika.MinionResult{
		"a": mr("a", papurika.StatusResponded, intp(0)),
		"b": mr("b", papurika.StatusNoResponse, nil),
	}, "a", "b")

	cases := []struct {
		name    string
		res     *papurika.Result
		policy  failPolicy
		allowNR bool
		want    int
	}{
		{"all ok / any", allOK, failAny, false, exitOK},
		{"partial / any", partial, failAny, false, exitPolicyFail},
		{"partial / all", partial, failAll, false, exitOK},
		{"allfail / all", allFail, failAll, false, exitPolicyFail},
		{"partial / none", partial, failNone, false, exitOK},
		{"noresponse counts / any", withNR, failAny, false, exitPolicyFail},
		{"noresponse allowed / any", withNR, failAny, true, exitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.res, nil, tc.policy, tc.allowNR); got != tc.want {
				t.Errorf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExitCodeErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{context.Canceled, exitInterrupted},
		{context.DeadlineExceeded, exitTimeout},
		{papurika.ErrAuthDenied, exitAuth},
		{papurika.ErrMalformed, exitData},
		{papurika.ErrTransport, exitUnavailable},
	}
	for _, tc := range cases {
		if got := exitCode(nil, tc.err, failAny, false); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestExitCodeNoMatch(t *testing.T) {
	// A true no-match: local command, no jid, no targeted set, no minions.
	empty := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocal, Fun: "test.ping",
	}}}
	if got := exitCode(empty, nil, failAny, false); got != exitNoMatch {
		t.Errorf("empty local result = %d, want %d (no match)", got, exitNoMatch)
	}
	// The JSON outcome must agree with the exit code (not the misleading "ok").
	if got := toView(empty, defaultOK).Summary.Outcome; got != "nomatch" {
		t.Errorf("no-match outcome = %q, want nomatch", got)
	}
}

func TestRunnerValueOutcomeOK(t *testing.T) {
	// A runner command (value-bearing, no minions) is "ok", not "nomatch".
	runnerRes := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientRunner, Fun: "manage.up",
		Value: papurika.NewRawValue(json.RawMessage(`["m1"]`)),
	}}}
	if got := toView(runnerRes, defaultOK).Summary.Outcome; got != "ok" {
		t.Errorf("runner outcome = %q, want ok", got)
	}
	if got := exitCode(runnerRes, nil, failAny, false); got != exitOK {
		t.Errorf("runner exit = %d, want 0", got)
	}
}

func TestRenderPrettyShapes(t *testing.T) {
	cases := []struct {
		name string
		cmd  papurika.CommandResult
		want []string
	}{
		{
			name: "highstate",
			cmd: papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
				Minions: map[string]papurika.MinionResult{"m1": {ID: "m1", Status: papurika.StatusResponded, Retcode: intp(0),
					Value: papurika.NewRawValue(json.RawMessage(`{"s_|-a":{"name":"a","result":true,"comment":"ok","duration":1.5,"__run_num__":0,"__id__":"a"},"s_|-b":{"result":false,"comment":"boom","__run_num__":1,"__id__":"b"}}`))}}},
			want: []string{"state.apply", "✓ a", "ok", "✗ b", "boom", "states"},
		},
		{
			name: "manage status up/down",
			cmd: papurika.CommandResult{Client: papurika.ClientRunner, Fun: "manage.status",
				Value: papurika.NewRawValue(json.RawMessage(`{"up":["m1"],"down":["m2"]}`))},
			want: []string{"● m1", "○ m2", "1 up · 1 down"},
		},
		{
			name: "key list",
			cmd: papurika.CommandResult{Client: papurika.ClientWheel, Fun: "wheel.key.list_all",
				Value: papurika.NewRawValue(json.RawMessage(`{"minions":["m1"],"minions_pre":["m2"],"minions_rejected":[],"minions_denied":[]}`))},
			want: []string{"accepted", "m1", "unaccepted", "m2"},
		},
		{
			name: "async submit",
			cmd: papurika.CommandResult{Client: papurika.ClientLocalAsync, Fun: "test.ping", JID: "9",
				Targeted: []string{"m1", "m2"}},
			want: []string{"submitted", "m1", "m2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderCommand(&buf, tc.cmd, glyphs{color: false})
			out := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q\n---\n%s", w, out)
				}
			}
		})
	}
}

func TestRenderJSONSchema(t *testing.T) {
	var buf bytes.Buffer
	r := res(map[string]papurika.MinionResult{"a": mr("a", papurika.StatusResponded, intp(0))}, "a")
	if err := render(&buf, r, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	var v resultView
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Schema != "papurika.dev/result/v1" {
		t.Errorf("schema = %q", v.Schema)
	}
	if v.Summary.Outcome != "ok" || v.Summary.Succeeded != 1 {
		t.Errorf("summary = %+v", v.Summary)
	}
}
