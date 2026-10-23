package main

import (
	"testing"

	"github.com/cstrahan/papurika"
)

func pendingResult(reason papurika.CompletionReason) *papurika.Result {
	return &papurika.Result{Commands: []papurika.CommandResult{{
		Client:   papurika.ClientLocalAsync,
		Fun:      "test.sleep",
		JID:      "20261023133607966019_51e09a3583",
		Targeted: []string{"m1", "m2"},
		Reason:   reason,
		Minions: map[string]papurika.MinionResult{
			"m1": {ID: "m1", Status: papurika.StatusResponded, Retcode: intp(0)},
			"m2": {ID: "m2", Status: papurika.StatusPending},
		},
	}}}
}

// A job that is simply still running has not failed. Counting its outstanding
// minions as no-response reported outcome "failed" and exit 1 for a perfectly
// healthy long job — the exact moment `papurika job <jid>` exists to reassure.
func TestPendingJobIsNotAFailure(t *testing.T) {
	res := pendingResult(papurika.ReasonNone)
	v := toView(res, defaultOK)
	if v.Summary.Pending != 1 {
		t.Errorf("summary.pending = %d, want 1", v.Summary.Pending)
	}
	if v.Summary.NoResponse != 0 {
		t.Errorf("summary.noresponse = %d, want 0 (pending is not no-response)", v.Summary.NoResponse)
	}
	if v.Summary.Outcome != "pending" {
		t.Errorf("outcome = %q, want %q", v.Summary.Outcome, "pending")
	}
	if got := exitCode(res, nil, failAny, false); got != exitOK {
		t.Errorf("exit = %d, want %d for a job that is merely unfinished", got, exitOK)
	}
}

// But a gather that ENDED with minions still pending (the jid aged out
// mid-run) must stay a failure — those minions will never return.
func TestAbandonedPendingStillFails(t *testing.T) {
	res := pendingResult(papurika.ReasonStreamFatal)
	if got := exitCode(res, nil, failAny, false); got != exitPolicyFail {
		t.Errorf("exit = %d, want %d for an abandoned gather", got, exitPolicyFail)
	}
}
