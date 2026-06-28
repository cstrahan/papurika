// Command papurika is a CLI for SaltStack's salt-api (a better pepper).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	// First SIGINT cancels the context (engine returns partial results + jid);
	// a second forces immediate exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := newRootCmd().ExecuteContext(ctx)

	var ee *exitErr
	if errors.As(err, &ee) {
		os.Exit(ee.code)
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
}
