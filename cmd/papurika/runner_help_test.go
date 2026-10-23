package main

import (
	"strings"
	"testing"
)

// The runner/wheel output shape is a documented gotcha: consumers need a second
// code path for it. Keep that in --help.
func TestRunnerHelpExplainsValueShape(t *testing.T) {
	root := newRootCmd()
	c, _, err := root.Find([]string{"runner"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Long, "commands[0].value") {
		t.Errorf("runner --help must say where the payload lands:\n%s", c.Long)
	}
}
