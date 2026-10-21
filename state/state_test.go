package state

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/cstrahan/papurika"
)

// All fixtures below are verbatim minion values from the captures in
// research/salt-test-cherrypy/out-state-shapes/ (the `ret` payload of the
// full_return envelope, which is what papurika puts in MinionResult.Value).

// fail_requisite_real.json — one state that ran and failed, and one that never
// ran because it required the first.
const fixtureRequisite = `{
  "cmd_|-prereq_fails_|-exit 1_|-run": {
    "name": "exit 1",
    "changes": {"pid": 64571, "retcode": 1, "stdout": "", "stderr": ""},
    "result": false,
    "comment": "Command \"exit 1\" run",
    "__sls__": "shapes.fail_requisite",
    "__run_num__": 0,
    "start_time": "23:25:05.857782",
    "duration": 14.282,
    "__id__": "prereq_fails"
  },
  "cmd_|-depends_on_prereq_|-echo should-not-run_|-run": {
    "changes": {},
    "result": false,
    "duration": 0.004,
    "start_time": "23:25:05.872387",
    "comment": "One or more requisite failed: shapes.fail_requisite.prereq_fails",
    "__state_ran__": false,
    "__run_num__": 1,
    "__sls__": "shapes.fail_requisite",
    "__id__": "depends_on_prereq",
    "name": "echo should-not-run"
  }
}`

// pkg_installed_would_test.json — test=True. Note result:null *with* a populated
// changes map: the would-change report is exactly a description of the changes.
const fixtureWouldChange = `{
  "pkg_|-shapes_pkg_installed_|-hello_|-installed": {
    "name": "hello",
    "changes": {"hello": {"new": "2.10-5build1", "old": ""}},
    "result": null,
    "comment": "The following packages would be installed/updated: hello",
    "__sls__": "shapes.pkg_installed",
    "__run_num__": 0,
    "start_time": "23:24:45.427500",
    "duration": 2574.954,
    "__id__": "shapes_pkg_installed"
  }
}`

// concurrency_01_sync_conflict.json — a second state run rejected because one is
// already in progress. The minion value is a list, not a states map.
const fixtureConflict = `["The function \"state.apply\" is running as PID 64639 and was started at 2026, Jul 09 23:26:36.523925 with jid 20260709232636523925"]`

// fullret_render_error.json — the SLS did not compile. Same list shape as a
// conflict, different cause. (Traceback elided; the first line is verbatim.)
const fixtureRenderError = `["Rendering SLS 'base:shapes.render_error' failed: Jinja error: division by zero"]`

func rawOf(t *testing.T, s string) papurika.RawValue {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("fixture is not valid JSON")
	}
	return papurika.NewRawValue(json.RawMessage(s))
}

func TestParseOrdersByRunNumAndClassifies(t *testing.T) {
	res, err := Parse(rawOf(t, fixtureRequisite))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(res.Entries))
	}
	// Salt's __run_num__ is execution order; the map iteration order is not.
	if got := res.Entries[0].StateID(); got != "prereq_fails" {
		t.Errorf("first entry = %q, want prereq_fails (sorted by __run_num__)", got)
	}
	if got := res.Entries[0].Outcome(); got != Failed {
		t.Errorf("entry 0 outcome = %v, want Failed", got)
	}
	// A skipped requisite is NOT a failure: it never ran. Conflating the two
	// misreports which state is actually broken.
	if got := res.Entries[1].Outcome(); got != Skipped {
		t.Errorf("entry 1 outcome = %v, want Skipped (__state_ran__ false)", got)
	}
	if n := len(res.Failed()); n != 1 {
		t.Errorf("Failed() = %d entries, want 1 (the skipped one must not count)", n)
	}
	if n := len(res.Skipped()); n != 1 {
		t.Errorf("Skipped() = %d entries, want 1", n)
	}
}

// result:null outranks a populated changes map. Checking changes first would
// report a test-mode preview as a real change.
func TestParseWouldChangeBeatsChanges(t *testing.T) {
	res, err := Parse(rawOf(t, fixtureWouldChange))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := res.Entries[0]
	if len(e.Changes) == 0 {
		t.Fatal("fixture should carry changes; it is the would-change payload")
	}
	if got := e.Outcome(); got != WouldChange {
		t.Errorf("outcome = %v, want WouldChange", got)
	}
	if n := len(res.Changed()); n != 0 {
		t.Errorf("Changed() = %d, want 0 — a test-mode preview is not a change", n)
	}
	if n := len(res.WouldChange()); n != 1 {
		t.Errorf("WouldChange() = %d, want 1", n)
	}
}

// The whole point of the package: a refused run must not decode to an empty
// Result, which would read as "no states ran, so nothing failed" — a clean run.
func TestParseConflictIsTypedErrorNotEmptyResult(t *testing.T) {
	res, err := Parse(rawOf(t, fixtureConflict))
	if err == nil {
		t.Fatalf("Parse returned no error; got result %+v — a refused run must not look clean", res)
	}
	if res != nil {
		t.Errorf("Parse returned a non-nil result alongside the error: %+v", res)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *ConflictError", err, err)
	}
	if ce.JID != "20260709232636523925" {
		t.Errorf("JID = %q, want 20260709232636523925", ce.JID)
	}
	if len(ce.Messages) != 1 {
		t.Errorf("Messages = %v, want the original message preserved", ce.Messages)
	}
}

// Same list shape, different cause — a compile failure must not be reported as a
// concurrency conflict, since the remedies are opposite (fix the SLS vs wait).
func TestParseCompileErrorIsNotAConflict(t *testing.T) {
	_, err := Parse(rawOf(t, fixtureRenderError))
	if err == nil {
		t.Fatal("Parse returned no error for an SLS that did not compile")
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("render error classified as a conflict: %v", err)
	}
	var compile *CompileError
	if !errors.As(err, &compile) {
		t.Fatalf("err = %v (%T), want *CompileError", err, err)
	}
	if len(compile.Messages) != 1 {
		t.Errorf("Messages = %v, want the render error preserved", compile.Messages)
	}
}

// retcode is salt's own verdict and is cheaper and more trustworthy than
// re-deriving one by scanning sub-states.
func TestOKPrefersRetcode(t *testing.T) {
	rc := func(n int) *int { return &n }

	for _, tc := range []struct {
		name    string
		retcode *int
		want    bool
	}{
		{"retcode 0 overrides a scan", rc(0), true},
		{"retcode 2 means ran-with-failures", rc(2), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseMinion(papurika.MinionResult{
				Value:   rawOf(t, fixtureWouldChange),
				Retcode: tc.retcode,
			})
			if err != nil {
				t.Fatalf("ParseMinion: %v", err)
			}
			if got := res.OK(); got != tc.want {
				t.Errorf("OK() = %v, want %v", got, tc.want)
			}
			if res.Retcode == nil || *res.Retcode != *tc.retcode {
				t.Errorf("Retcode not carried through: %v", res.Retcode)
			}
		})
	}

	t.Run("falls back to scanning when salt reported none", func(t *testing.T) {
		res, err := Parse(rawOf(t, fixtureRequisite))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if res.OK() {
			t.Error("OK() = true, want false (a failed state with no retcode)")
		}
	})
}

func TestEntryTagAccessors(t *testing.T) {
	res, err := Parse(rawOf(t, fixtureWouldChange))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := res.Entries[0]
	for _, tc := range []struct{ name, got, want string }{
		{"Module", e.Module(), "pkg"},
		{"Function", e.Function(), "installed"},
		{"StateID", e.StateID(), "shapes_pkg_installed"},
		{"TargetName", e.TargetName(), "hello"},
		{"SLS", e.SLS, "shapes.pkg_installed"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// With no __id__/name, the accessors fall back to the tag segments.
func TestEntryTagFallback(t *testing.T) {
	res, err := Parse(rawOf(t, `{"file_|-cfg_|-/etc/x_|-managed": {"result": true, "__run_num__": 0}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e := res.Entries[0]
	if e.StateID() != "cfg" || e.TargetName() != "/etc/x" {
		t.Errorf("tag fallback: id=%q name=%q, want cfg and /etc/x", e.StateID(), e.TargetName())
	}
	if e.Outcome() != Clean {
		t.Errorf("outcome = %v, want Clean (result true, no changes)", e.Outcome())
	}
}

func TestParseRejectsUnusableInput(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"empty", ``},
		{"whitespace only", "  \n\t"},
		{"a scalar is not a state return", `"nope"`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(papurika.NewRawValue(json.RawMessage(tc.in))); err == nil {
				t.Errorf("Parse(%q) returned no error", tc.in)
			}
		})
	}
}

func TestLooks(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"states map", fixtureWouldChange, true},
		{"error list", fixtureConflict, true},
		{"a plain exec return", `"pong"`, false},
		{"a map without __run_num__", `{"a": {"b": 1}}`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Looks(papurika.NewRawValue(json.RawMessage(tc.in))); got != tc.want {
				t.Errorf("Looks() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOutcomeString(t *testing.T) {
	for o, want := range map[Outcome]string{
		Clean: "clean", Changed: "changed", Failed: "failed",
		Skipped: "skipped", WouldChange: "would-change", Outcome(99): "unknown",
	} {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, want)
		}
	}
}
