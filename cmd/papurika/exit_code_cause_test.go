package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/cstrahan/papurika"
)

// A deadline arrives wrapped in ErrTransport (the HTTP exchange is what the
// clock interrupts). It must still map to 124, not 69.
func TestDeadlineInsideTransportErrorExits124(t *testing.T) {
	err := fmt.Errorf("%w: %w", papurika.ErrTransport, context.DeadlineExceeded)
	if got := exitCode(nil, err, failAny, false); got != exitTimeout {
		t.Errorf("exit = %d, want %d", got, exitTimeout)
	}
	cancel := fmt.Errorf("%w: %w", papurika.ErrTransport, context.Canceled)
	if got := exitCode(nil, cancel, failAny, false); got != exitInterrupted {
		t.Errorf("exit = %d, want %d", got, exitInterrupted)
	}
}
