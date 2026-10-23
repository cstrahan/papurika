package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cstrahan/papurika"
	"github.com/cstrahan/papurika/schema"
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
	if got := schema.FromResult(empty).Summary.Outcome; got != "nomatch" {
		t.Errorf("no-match outcome = %q, want nomatch", got)
	}
}

func TestRunnerValueOutcomeOK(t *testing.T) {
	// A runner command (value-bearing, no minions) is "ok", not "nomatch".
	runnerRes := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientRunner, Fun: "manage.up",
		Value: papurika.NewRawValue(json.RawMessage(`["m1"]`)),
	}}}
	if got := schema.FromResult(runnerRes).Summary.Outcome; got != "ok" {
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
			renderCommand(&buf, tc.cmd, renderOpts{}, glyphs{color: false})
			out := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q\n---\n%s", w, out)
				}
			}
		})
	}
}

func TestRenderYAML(t *testing.T) {
	var buf bytes.Buffer
	r := res(map[string]papurika.MinionResult{"a": mr("a", papurika.StatusResponded, intp(0))}, "a")
	if err := render(&buf, r, outputYAML, renderOpts{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, w := range []string{"schema: papurika.dev/result/v1", "status: responded", "outcome: ok"} {
		if !strings.Contains(out, w) {
			t.Errorf("yaml output missing %q\n---\n%s", w, out)
		}
	}
	if strings.Contains(out, "{") {
		t.Errorf("yaml output should be block style, not JSON:\n%s", out)
	}
}

func TestWriteTree(t *testing.T) {
	var buf bytes.Buffer
	writeTree(&buf, map[string]any{
		"os":       "Ubuntu",
		"num_cpus": float64(4),
		"ipv4":     []any{"10.0.0.1"},
		"nested":   map[string]any{"k": "v"},
		"truthy":   true,
		"emptyVal": nil,
	}, "")
	out := buf.String()
	for _, w := range []string{"os: Ubuntu", "num_cpus: 4", "- 10.0.0.1", "nested:", "  k: v", "truthy: true", "emptyVal: null"} {
		if !strings.Contains(out, w) {
			t.Errorf("tree missing %q\n---\n%s", w, out)
		}
	}
}

// parseLines splits NDJSON output into decoded objects, asserting each is valid.
func parseLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

func TestWriteNDJSONComplete(t *testing.T) {
	r := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocalAsync, Fun: "test.ping", JID: "42",
		Targeted: []string{"m1", "m2"},
		Minions: map[string]papurika.MinionResult{
			"m1": mr("m1", papurika.StatusResponded, intp(0)),
			"m2": mr("m2", papurika.StatusNoResponse, nil),
		},
	}}}
	var buf bytes.Buffer
	if err := writeNDJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	lines := parseLines(t, buf.Bytes())
	if lines[0]["type"] != "submitted" || lines[0]["jid"] != "42" {
		t.Errorf("first line must be submitted+jid, got %v", lines[0])
	}
	if lines[len(lines)-1]["type"] != "summary" {
		t.Errorf("last line must be summary, got %v", lines[len(lines)-1])
	}
}

func TestNDJSONStreamerSubmittedFirstAndBackfill(t *testing.T) {
	var buf bytes.Buffer
	st := newNDJSONStreamer(&buf)

	job := papurika.Job{JID: "99", Targeted: []string{"m1", "m2"}}
	st.onSubmit(job)
	st.onUpdate(papurika.Update{JID: "99", Minion: mr("m1", papurika.StatusResponded, intp(0))})
	// m2 never streamed; finish must backfill it from the result, then summary.
	res := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocalAsync, Fun: "test.ping", JID: "99",
		Targeted: []string{"m1", "m2"},
		Minions: map[string]papurika.MinionResult{
			"m1": mr("m1", papurika.StatusResponded, intp(0)),
			"m2": mr("m2", papurika.StatusNoResponse, nil),
		},
	}}}
	st.finish(res, nil)

	lines := parseLines(t, buf.Bytes())
	if lines[0]["type"] != "submitted" || lines[0]["jid"] != "99" {
		t.Fatalf("first line must carry the jid (no-lost-jid), got %v", lines[0])
	}
	// Count minion lines: m1 (streamed) + m2 (backfilled) = 2, no dup of m1.
	var minions, summaries int
	seen := map[string]bool{}
	for _, l := range lines {
		switch l["type"] {
		case "minion":
			id, _ := l["id"].(string)
			if seen[id] {
				t.Errorf("minion %s emitted twice", id)
			}
			seen[id] = true
			minions++
		case "summary":
			summaries++
		}
	}
	if minions != 2 || !seen["m1"] || !seen["m2"] {
		t.Errorf("want 2 distinct minion lines (m1,m2), got %d %v", minions, seen)
	}
	if summaries != 1 || lines[len(lines)-1]["type"] != "summary" {
		t.Errorf("want exactly one trailing summary line")
	}
}

func TestRenderJSONSchema(t *testing.T) {
	var buf bytes.Buffer
	r := res(map[string]papurika.MinionResult{"a": mr("a", papurika.StatusResponded, intp(0))}, "a")
	if err := render(&buf, r, outputJSON, renderOpts{}); err != nil {
		t.Fatal(err)
	}
	var v schema.Result
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Schema != schema.Version {
		t.Errorf("schema = %q", v.Schema)
	}
	if v.Summary.Outcome != "ok" || v.Summary.Succeeded != 1 {
		t.Errorf("summary = %+v", v.Summary)
	}
}

// --- highstate renderer (design/cli-output-format.md §D) ---

func hsMinion(id string, rc int, value string) papurika.MinionResult {
	return papurika.MinionResult{
		ID: id, Status: papurika.StatusResponded, Retcode: intp(rc),
		Value: papurika.NewRawValue(json.RawMessage(value)),
	}
}

func renderCmdStr(c papurika.CommandResult, opts renderOpts) string {
	var buf bytes.Buffer
	renderCommand(&buf, c, opts, glyphs{color: false})
	return buf.String()
}

// A mixed highstate: clean, changed (old→new), failed, file (New file diff), cmd.
const mixedHighstate = `{
 "test_|-mixed_clean_|-mixed_clean_|-succeed_without_changes":{"name":"mixed_clean","result":true,"comment":"Success!","changes":{},"__run_num__":0,"__id__":"mixed_clean","duration":0.6},
 "test_|-mixed_changed_|-mixed_changed_|-succeed_with_changes":{"name":"mixed_changed","result":true,"comment":"Success!","changes":{"testing":{"old":"Unchanged","new":"Something pretended to change"}},"__run_num__":1,"__id__":"mixed_changed","duration":0.3},
 "test_|-mixed_failed_|-mixed_failed_|-fail_without_changes":{"name":"mixed_failed","result":false,"comment":"Failure!","changes":{},"__run_num__":2,"__id__":"mixed_failed","duration":0.2},
 "file_|-mixed_file_|-/tmp/shapes/mixed.txt_|-managed":{"name":"/tmp/shapes/mixed.txt","result":true,"comment":"File /tmp/shapes/mixed.txt updated","changes":{"diff":"New file","mode":"0644"},"__run_num__":3,"__id__":"mixed_file","duration":14},
 "cmd_|-mixed_command_|-echo hi_|-run":{"name":"echo hi","result":true,"comment":"Command run","changes":{"pid":661,"retcode":0,"stdout":"running the mixed command","stderr":""},"__run_num__":4,"__id__":"mixed_command","duration":5}
}`

func TestRenderHighstateMixed(t *testing.T) {
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 2, mixedHighstate)}}
	out := renderCmdStr(c, renderOpts{})

	for _, want := range []string{
		"✓ mixed_clean", "test.succeed_without_changes",
		"✓ mixed_changed", "testing: Unchanged → Something pretended to change",
		"✗ mixed_failed", "Failure!",
		"✓ mixed_file", "+ new file", "mode: 0644",
		"✓ mixed_command", "stdout: running the mixed command",
		"5 states", "4 ok (3 changed)", "1 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
	// Ordering follows __run_num__: clean before command.
	if strings.Index(out, "mixed_clean") > strings.Index(out, "mixed_command") {
		t.Errorf("states not ordered by __run_num__\n%s", out)
	}
	// A failed state must not be counted as "changed".
	if strings.Contains(out, "4 changed") || strings.Contains(out, "(4 changed)") {
		t.Errorf("failed-with-changes should not inflate changed count\n%s", out)
	}
}

func TestRenderHighstateChangesOnly(t *testing.T) {
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 2, mixedHighstate)}}
	out := renderCmdStr(c, renderOpts{changesOnly: true})
	if strings.Contains(out, "mixed_clean") {
		t.Errorf("--changes-only should hide the clean state\n%s", out)
	}
	for _, want := range []string{"mixed_changed", "mixed_failed", "mixed_file"} {
		if !strings.Contains(out, want) {
			t.Errorf("--changes-only dropped a non-clean state %q\n%s", want, out)
		}
	}
	// Footer counts still reflect every state.
	if !strings.Contains(out, "5 states") {
		t.Errorf("footer should still count all states\n%s", out)
	}
}

func TestRenderHighstateSkippedRequisite(t *testing.T) {
	const v = `{
	 "cmd_|-prereq_fails_|-exit 1_|-run":{"name":"exit 1","result":false,"comment":"Command run","changes":{"pid":1,"retcode":1,"stdout":"","stderr":""},"__run_num__":0,"__id__":"prereq_fails","duration":7},
	 "cmd_|-depends_|-echo x_|-run":{"name":"echo x","result":false,"comment":"One or more requisite failed: shapes.fail_requisite.prereq_fails","changes":{},"__run_num__":1,"__id__":"depends","__state_ran__":false,"duration":0.004}
	}`
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 2, v)}}
	out := renderCmdStr(c, renderOpts{})
	for _, want := range []string{
		"✗ prereq_fails", "exit 1",
		"⊘ depends", "requisite failed: shapes.fail_requisite.prereq_fails",
		"1 failed", "1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHighstateCompileError(t *testing.T) {
	// A state.* command whose minion value is a list still dispatches to the
	// highstate renderer (via the fun prefix), even with no dict to sniff.
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{
			"m1": hsMinion("m1", 1, `["No matching sls found for 'nope' in env 'base'"]`)}}
	out := renderCmdStr(c, renderOpts{})
	for _, want := range []string{"did not compile", "No matching sls found for 'nope'", "1 did not compile"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHighstateConflict(t *testing.T) {
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{
			"m1": hsMinion("m1", 1, `["The function \"state.apply\" is running as PID 63027 and was started at 2026, Jul 09 23:17:30.801316 with jid 20260709231730801316"]`)}}
	out := renderCmdStr(c, renderOpts{})
	for _, want := range []string{
		"a state run is already in progress",
		"20260709231730801316",
		"papurika watch 20260709231730801316",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHighstateWouldChange(t *testing.T) {
	// test=True: result null with a pending diff. Glyph ~, counted as would-change.
	const v = `{
	 "file_|-f_|-/tmp/x_|-managed":{"name":"/tmp/x","result":null,"comment":"The file /tmp/x is set to be changed","changes":{"diff":"New file"},"__run_num__":0,"__id__":"f","duration":1}
	}`
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 0, v)}}
	out := renderCmdStr(c, renderOpts{})
	for _, want := range []string{"~ f", "+ new file", "1 would-change"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHighstateCleanRemovalDedup(t *testing.T) {
	// file.directory clean:True — salt returns each path both per-path and in an
	// aggregate list; the formatter consolidates to one removed: list.
	const real = `{
	 "file_|-clean_|-/etc/coronerd/services.d/_|-directory":{"name":"/etc/coronerd/services.d/","result":true,"comment":"Files cleaned","changes":{
	   "/etc/coronerd/services.d/notification.json":{"removed":"Removed due to clean"},
	   "removed":["/etc/coronerd/services.d/notification.json"]},"__run_num__":0,"__id__":"clean","duration":3}
	}`
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 0, real)}}
	out := renderCmdStr(c, renderOpts{})
	// The consolidated list is present exactly once...
	if strings.Count(out, "/etc/coronerd/services.d/notification.json") != 1 {
		t.Errorf("path should appear exactly once (deduped)\n---\n%s", out)
	}
	// ...and the noisy per-path marker is gone.
	if strings.Contains(out, "Removed due to clean") {
		t.Errorf("per-path 'Removed due to clean' marker should be dropped\n---\n%s", out)
	}
	if !strings.Contains(out, "removed:") {
		t.Errorf("consolidated removed: list missing\n---\n%s", out)
	}
}

func TestRenderHighstateCleanRemovalTestMode(t *testing.T) {
	// test=True: only per-path entries, no aggregate list — still consolidate.
	const tm = `{
	 "file_|-clean_|-/d/_|-directory":{"name":"/d/","result":null,"comment":"would change","changes":{
	   "/d/a.json":{"removed":"Removed due to clean"},
	   "/d/b.json":{"removed":"Removed due to clean"}},"__run_num__":0,"__id__":"clean","duration":1}
	}`
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "state.apply", JID: "1",
		Minions: map[string]papurika.MinionResult{"m1": hsMinion("m1", 0, tm)}}
	out := renderCmdStr(c, renderOpts{})
	if strings.Contains(out, "Removed due to clean") {
		t.Errorf("test-mode per-path marker should be consolidated too\n---\n%s", out)
	}
	for _, want := range []string{"removed:", "- /d/a.json", "- /d/b.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestConsolidateChangesNoOp(t *testing.T) {
	// file.absent's {removed: "<path>"} (string value) and ordinary changes are
	// left untouched.
	absent := map[string]any{"removed": "/tmp/goner.txt"}
	if got := consolidateChanges(absent); len(got) != 1 || got["removed"] != "/tmp/goner.txt" {
		t.Errorf("file.absent changes altered: %+v", got)
	}
	diff := map[string]any{"diff": "New file", "mode": "0644"}
	got := consolidateChanges(diff)
	if got["diff"] != "New file" || got["mode"] != "0644" || len(got) != 2 {
		t.Errorf("diff changes altered: %+v", got)
	}
}

// --- exec value rendering (design/cli-output-format.md §C) ---

func execMinion(id string, rc int, jsonValue string) papurika.MinionResult {
	return papurika.MinionResult{ID: id, Status: papurika.StatusResponded, Retcode: intp(rc),
		Value: papurika.NewRawValue(json.RawMessage(jsonValue))}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func execCmd(m papurika.MinionResult) papurika.CommandResult {
	return papurika.CommandResult{Client: papurika.ClientLocal, Fun: "cmd.run", JID: "1",
		Minions: map[string]papurika.MinionResult{m.ID: m}}
}

func TestRenderExecMultilineFull(t *testing.T) {
	out := renderCmdStr(execCmd(execMinion("m1", 0, jsonStr("tada\nfoobar"))), renderOpts{})
	// Full output, not truncated, as an indented block.
	if strings.Contains(out, "…") {
		t.Errorf("output must not be truncated\n%s", out)
	}
	for _, want := range []string{"✓ m1", "      tada", "      foobar"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderExecFailedFull(t *testing.T) {
	out := renderCmdStr(execCmd(execMinion("m1", 7, jsonStr("line1\nline2 boom"))), renderOpts{})
	for _, want := range []string{"✗ m1", "(retcode 7)", "line1", "line2 boom", "1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderExecShortInline(t *testing.T) {
	out := renderCmdStr(execCmd(execMinion("m1", 0, jsonStr("one-line"))), renderOpts{})
	if !strings.Contains(out, "✓ m1   one-line") {
		t.Errorf("short single-line value should be inline\n%s", out)
	}
}

func TestRenderExecBoolAndTree(t *testing.T) {
	// test.ping-style bool → True inline.
	out := renderCmdStr(execCmd(execMinion("m1", 0, "true")), renderOpts{})
	if !strings.Contains(out, "✓ m1   True") {
		t.Errorf("bool should render as True inline\n%s", out)
	}
	// grains-style map → tree.
	out = renderCmdStr(execCmd(execMinion("m1", 0, `{"os":"Ubuntu","num_cpus":4}`)), renderOpts{})
	for _, want := range []string{"✓ m1", "      num_cpus: 4", "      os: Ubuntu"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

// --- jid on stdout (never-lose-a-jid via the result, not stderr) ---

func TestJSONSummaryCarriesJIDs(t *testing.T) {
	var buf bytes.Buffer
	r := &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocal, Fun: "test.ping", JID: "20260710X",
		Targeted: []string{"a"}, Minions: map[string]papurika.MinionResult{
			"a": mr("a", papurika.StatusResponded, intp(0))},
	}}}
	if err := render(&buf, r, outputJSON, renderOpts{}); err != nil {
		t.Fatal(err)
	}
	var v schema.Result
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Summary.JIDs) != 1 || v.Summary.JIDs[0] != "20260710X" {
		t.Errorf("summary.jids = %v, want [20260710X]", v.Summary.JIDs)
	}
}

func TestPrettyPartialShowsJIDAndResume(t *testing.T) {
	// An interrupted command (Reason=Canceled) must carry its jid on stdout and a
	// resume hint — the recovery affordance lives here, not on stderr.
	c := papurika.CommandResult{
		Client: papurika.ClientLocal, Fun: "state.highstate", JID: "20260710Z",
		Targeted: []string{"a", "b"}, Reason: papurika.ReasonCanceled,
		Minions: map[string]papurika.MinionResult{
			"a": mr("a", papurika.StatusResponded, intp(0)),
			"b": {ID: "b", Status: papurika.StatusPending},
		},
	}
	out := renderCmdStr(c, renderOpts{})
	for _, want := range []string{"(jid 20260710Z)", "PARTIAL", "resume: papurika watch 20260710Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}

func TestPrettyCompleteHasNoPartial(t *testing.T) {
	// A normal sync command (Reason=None) must NOT be labeled PARTIAL.
	c := papurika.CommandResult{Client: papurika.ClientLocal, Fun: "test.ping", JID: "1",
		Minions: map[string]papurika.MinionResult{"a": mr("a", papurika.StatusResponded, intp(0))}}
	out := renderCmdStr(c, renderOpts{})
	if strings.Contains(out, "PARTIAL") || strings.Contains(out, "resume:") {
		t.Errorf("completed command should not show PARTIAL/resume\n%s", out)
	}
}
