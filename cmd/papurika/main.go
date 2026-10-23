// Command papurika is a CLI for SaltStack's salt-api (a better pepper).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// termSignal records that the run was ended by SIGTERM rather than SIGINT, so
// the exit code can be 143 instead of 130. Both cancel the context, and
// context.Canceled alone cannot tell them apart.
var termSignal atomic.Bool

func main() {
	ctx, stop := watchSignals(context.Background())
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)

	var ee *exitErr
	if errors.As(err, &ee) {
		os.Exit(signalAdjusted(ee.code))
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(os.Stderr, "papurika: %s\n", ue.msg)
		os.Exit(exitUsage)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "papurika: %v\n", err)
		os.Exit(exitUsage) // cobra arg/flag errors land here
	}
	os.Exit(signalAdjusted(exitOK))
}

// watchSignals cancels the returned context on the first SIGINT or SIGTERM, and
// forces an immediate exit on the second.
//
// SIGTERM matters as much as SIGINT here: anything running papurika under
// timeout(1), a CI job, or a supervisor is killed with SIGTERM, and ignoring it
// meant the process died with nothing on stdout — losing the jid, which the
// engine goes to some length to guarantee it never does (design/async.md §C).
//
// It is also why this is not signal.NotifyContext: that cancels on the first
// signal but leaves the handler installed, so a second Ctrl-C re-cancels an
// already-cancelled context and does nothing. design/cli.md §E promises a second
// interrupt exits immediately without draining; only an explicit handler can.
func watchSignals(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig, ok := <-ch
		if !ok {
			return
		}
		if sig == syscall.SIGTERM {
			termSignal.Store(true)
		}
		cancel()
		// Second signal: the operator asked twice. Skip the drain and go.
		if _, ok := <-ch; ok {
			os.Exit(signalAdjusted(exitInterrupted))
		}
	}()
	return ctx, func() { signal.Stop(ch); close(ch); cancel() }
}

// signalAdjusted maps the SIGINT exit code onto SIGTERM's when that is the
// signal we actually got (128 + 15, matching the shell convention that 130 is
// 128 + 2). Any other code — a real auth or policy failure that happened to
// race the signal — is reported as-is.
func signalAdjusted(code int) int {
	if code == exitInterrupted && termSignal.Load() {
		return exitTerminated
	}
	return code
}
