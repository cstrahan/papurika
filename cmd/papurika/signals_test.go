package main

import (
	"testing"
)

// SIGTERM must be reported as 143, not SIGINT's 130, and must not relabel an
// unrelated exit code.
func TestSignalAdjusted(t *testing.T) {
	termSignal.Store(false)
	if got := signalAdjusted(exitInterrupted); got != exitInterrupted {
		t.Errorf("without SIGTERM: %d, want %d", got, exitInterrupted)
	}
	termSignal.Store(true)
	defer termSignal.Store(false)
	if got := signalAdjusted(exitInterrupted); got != exitTerminated {
		t.Errorf("with SIGTERM: %d, want %d", got, exitTerminated)
	}
	if got := signalAdjusted(exitAuth); got != exitAuth {
		t.Errorf("an auth failure racing a signal must stay %d, got %d", exitAuth, got)
	}
}
