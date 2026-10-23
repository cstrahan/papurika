package main

import (
	"testing"
)

// Client-side batching is the default: it is the only batch mode that keeps a
// recoverable jid. --batch alone must never fall through to server-side
// local_batch, which returns none.
func TestBatchDefaultsToClientSide(t *testing.T) {
	root := newRootCmd()
	run, _, err := root.Find([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Flags().Lookup("batch-mode").DefValue; got != batchModeClient {
		t.Errorf("--batch-mode default = %q, want %q", got, batchModeClient)
	}

	tg := &targeting{batch: "10", batchMode: batchModeClient}
	if got := batchIfServer(tg); got != "" {
		t.Errorf("lowstate batch = %q, want empty under client-side batching", got)
	}
	if len(tg.batchOpts()) == 0 {
		t.Error("client-side batching must produce a WithClientBatch option")
	}

	srv := &targeting{batch: "10", batchMode: batchModeServer}
	if got := batchIfServer(srv); got != "10" {
		t.Errorf("lowstate batch = %q, want 10 under --batch-mode server", got)
	}
	if len(srv.batchOpts()) != 0 {
		t.Error("server-side batching must not add client-batch options")
	}
}

func TestBatchModeRejectsUnknownValue(t *testing.T) {
	tg := &targeting{batch: "10", batchMode: "serverside"}
	if _, err := tg.resolve(); err == nil {
		t.Fatal("expected a usage error for an unknown --batch-mode")
	}
}

// The low-level tier maps one client to one subcommand with no smart defaults,
// so it must not offer the engine-level batching knobs.
func TestLowLevelCommandsHaveNoBatchMode(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"local", "local-batch", "ssh"} {
		c, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if c.Flags().Lookup("batch-mode") != nil {
			t.Errorf("%s must not take --batch-mode", name)
		}
	}
	lb, _, _ := root.Find([]string{"local-batch"})
	if lb.Flags().Lookup("batch") == nil {
		t.Error("local-batch still needs --batch")
	}
}
