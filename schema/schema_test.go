package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cstrahan/papurika"
)

// jsonKeys returns the "name,opts" tag of every field of a struct type, so a
// rename or removal is visible as a diff rather than as a consumer breaking in
// production.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json tag; every field of a v1 type is part of the contract",
				rt.Name(), rt.Field(i).Name)
		}
		out = append(out, tag)
	}
	return out
}

// The descriptor in jsonschema.go claims to describe the v1 types. This is
// what stops it from being a lie: every struct field must have a descriptor
// entry with the identical json tag, in the same order, and vice versa.
//
// That cross-check is the whole reason the descriptor can be trusted to
// generate a JSON Schema. Adding a struct field without describing it fails
// here; so does describing a field that does not exist.
func TestV1FieldSetIsPinned(t *testing.T) {
	for _, o := range v1Objects {
		got := jsonKeys(t, o.goType)
		want := make([]string, len(o.fields))
		for i, f := range o.fields {
			want[i] = f.tag
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: struct fields and schema descriptor disagree:\n struct     %q\n descriptor %q\n"+
				"Adding a property is fine in v1 — describe it in v1Objects and note it in the changelog. "+
				"Renaming or removing one is a v2.", o.name, got, want)
		}
	}
}

// Every exported type in the package must be described. A new one that nobody
// adds to v1Objects would be invisible in the JSON Schema while being very
// visible in the output.
func TestEveryV1TypeIsDescribed(t *testing.T) {
	described := map[string]bool{}
	for _, o := range v1Objects {
		described[reflect.TypeOf(o.goType).Name()] = true
	}
	for _, want := range []string{"Result", "Summary", "Command", "Minion", "Error"} {
		if !described[want] {
			t.Errorf("type %s has no entry in v1Objects, so it is missing from the JSON Schema", want)
		}
	}
}

// Minion.Retcode and Minion.Return must NOT be omitempty: a consumer indexing
// m["retcode"] has to find null rather than a missing key, because "did not
// report a retcode" and "reported 0" are different facts.
func TestNullBearingFieldsAreAlwaysPresent(t *testing.T) {
	b, err := json.Marshal(Minion{ID: "m1", Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "status", "retcode", "return"} {
		if _, ok := m[k]; !ok {
			t.Errorf("minion key %q is absent; it must be present (null if unknown): %s", k, b)
		}
	}
}

func rc(i int) *int { return &i }

func sampleResult() *papurika.Result {
	return &papurika.Result{Commands: []papurika.CommandResult{{
		Client:    papurika.ClientLocalAsync,
		Fun:       "test.ping",
		JID:       "20260101000000000001",
		ExtraJIDs: []string{"20260101000000000002"},
		Targeted:  []string{"m1", "m2", "m3", "m4"},
		Minions: map[string]papurika.MinionResult{
			"m2": {ID: "m2", Status: papurika.StatusResponded, Retcode: rc(7)},
			"m1": {ID: "m1", Status: papurika.StatusResponded, Retcode: rc(0)},
			"m4": {ID: "m4", Status: papurika.StatusPending},
			"m3": {ID: "m3", Status: papurika.StatusNoResponse},
		},
	}}}
}

// A document must round-trip: what FromResult produces is exactly what a
// consumer unmarshals back. This is the promise the package exists to make.
func TestRoundTrip(t *testing.T) {
	want := FromResult(sampleResult())
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Result
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("papurika's own output does not unmarshal into its own type: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip differs:\n got  %+v\n want %+v", got, want)
	}
}

// Minion order must not depend on Go map iteration, or two runs of the same
// job produce different bytes and any diff-based consumer sees noise.
func TestMinionOrderIsStable(t *testing.T) {
	first, _ := json.Marshal(FromResult(sampleResult()))
	for i := 0; i < 20; i++ {
		again, _ := json.Marshal(FromResult(sampleResult()))
		if string(again) != string(first) {
			t.Fatalf("output is not deterministic across runs:\n %s\n %s", first, again)
		}
	}
	doc := FromResult(sampleResult())
	ids := make([]string, 0, len(doc.Commands[0].Minions))
	for _, m := range doc.Commands[0].Minions {
		ids = append(ids, m.ID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("minions are not sorted by id: %v", ids)
	}
}

// The summary's partitions must actually add up, or the counts a CI job
// branches on are arithmetic nonsense.
func TestSummaryPartitionsAddUp(t *testing.T) {
	s := FromResult(sampleResult()).Summary
	if got := s.Returned + s.NoResponse + s.Pending; got != s.Targeted {
		t.Errorf("returned+noresponse+pending = %d, want targeted = %d (%+v)", got, s.Targeted, s)
	}
	if got := s.Succeeded + s.Failed; got != s.Returned {
		t.Errorf("succeeded+failed = %d, want returned = %d (%+v)", got, s.Returned, s)
	}
	if s.Outcome != OutcomePartial {
		t.Errorf("outcome = %q, want %q", s.Outcome, OutcomePartial)
	}
	// Every jid must be reachable from the summary, including a batched run's
	// extra chunks — the never-lose-a-jid rule on the machine path.
	if len(s.JIDs) != 2 {
		t.Errorf("summary.jids = %v, want both the primary and the extra", s.JIDs)
	}
}

// commands is iterated unconditionally by consumers; null would break that.
func TestCommandsIsNeverNull(t *testing.T) {
	for name, doc := range map[string]Result{
		"empty result": FromResult(&papurika.Result{}),
		"nil result":   FromResult(nil),
		"error only":   FromError(fmt.Errorf("boom")),
	} {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"commands":null`) {
			t.Errorf("%s: commands is null, want []: %s", name, b)
		}
	}
}

func TestMinionOK(t *testing.T) {
	cases := []struct {
		m    Minion
		want bool
	}{
		{Minion{Status: StatusResponded, Retcode: rc(0)}, true},
		// No retcode reported still counts as ok — the half that is easy to
		// get wrong when reimplementing this.
		{Minion{Status: StatusResponded}, true},
		{Minion{Status: StatusResponded, Retcode: rc(1)}, false},
		{Minion{Status: StatusNoResponse}, false},
		{Minion{Status: StatusPending}, false},
		{Minion{Status: StatusError}, false},
	}
	for _, c := range cases {
		if got := c.m.OK(); got != c.want {
			t.Errorf("Minion%+v.OK() = %v, want %v", c.m, got, c.want)
		}
	}
}

// A deadline reaches papurika wrapped in ErrTransport; reporting it as
// "transport" would lose the only part a caller can act on.
func TestErrorKind(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %w", papurika.ErrTransport, context.DeadlineExceeded), ErrKindTimeout},
		{fmt.Errorf("%w: %w", papurika.ErrTransport, context.Canceled), ErrKindInterrupted},
		{fmt.Errorf("%w: dial", papurika.ErrTransport), ErrKindTransport},
		{papurika.ErrAuthFailure, ErrKindAuthFailure},
		{papurika.ErrAuthDenied, ErrKindAuthDenied},
		{papurika.ErrMalformed, ErrKindMalformed},
		{papurika.ErrServerError, ErrKindServerError},
		{papurika.ErrJobNotFound, ErrKindJobNotFound},
		{papurika.ErrUnsupported, ErrKindUnsupported},
		{fmt.Errorf("something else"), ErrKindOther},
	}
	for _, c := range cases {
		if got := ErrorKind(c.err); got != c.want {
			t.Errorf("ErrorKind(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if NewError(nil) != nil {
		t.Error("NewError(nil) must be nil, so a successful document has no error object")
	}
	// *Error is usable as an error, which is the point of the method.
	var err error = &Error{Kind: ErrKindTimeout, Message: "timed out"}
	if err.Error() != "timed out" {
		t.Errorf("Error() = %q", err.Error())
	}
}

// Every status papurika can produce must have a string constant here, or a
// consumer branching on the documented set silently misses one.
func TestEveryLibraryStatusIsNamed(t *testing.T) {
	named := map[string]bool{
		StatusResponded: true, StatusNoResponse: true, StatusPending: true,
		StatusError: true, StatusNotMatched: true,
	}
	for s := papurika.StatusPending; s <= papurika.StatusNotMatched; s++ {
		if !named[s.String()] {
			t.Errorf("papurika.Status %q has no constant in schema; add one", s)
		}
	}
}
