package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cstrahan/papurika"
	"github.com/cstrahan/papurika/schema"
)

// A request-level failure must still produce a parseable document in the
// machine formats. Emitting zero bytes made json.load() raise on exactly the
// runs a script most needs to inspect.
func TestErrorEnvelopeIsParseable(t *testing.T) {
	var buf bytes.Buffer
	err := fmt.Errorf("%w: %w", papurika.ErrTransport, context.DeadlineExceeded)
	renderErrorOnly(&buf, outputJSON, err)

	var v map[string]any
	if jerr := json.Unmarshal(buf.Bytes(), &v); jerr != nil {
		t.Fatalf("error output is not valid JSON (%v): %q", jerr, buf.String())
	}
	if v["schema"] != schema.Version {
		t.Errorf("schema = %v", v["schema"])
	}
	if sum := v["summary"].(map[string]any); sum["outcome"] != "error" {
		t.Errorf("outcome = %v, want error", sum["outcome"])
	}
	if _, ok := v["commands"].([]any); !ok {
		t.Errorf("commands must be a list, got %T", v["commands"])
	}
	ev, ok := v["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %q", buf.String())
	}
	if ev["kind"] != "timeout" {
		t.Errorf("error.kind = %v, want timeout", ev["kind"])
	}
}
