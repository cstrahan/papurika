package schema

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cstrahan/papurika"
)

var update = flag.Bool("update", false, "rewrite the golden JSON Schema in testdata")

const goldenPath = "testdata/result-v1.schema.json"

// The generated schema is committed, so any change to the contract — a
// property, a type, a description, a known vocabulary value — shows up as a
// reviewable diff rather than as something a reader has to reconstruct from a
// descriptor literal.
//
// Regenerate with: go test ./schema -update
func TestJSONSchemaMatchesGolden(t *testing.T) {
	got := JSONSchema()
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v\nrun `go test ./schema -update` to create it", err)
	}
	if got != string(want) {
		t.Errorf("generated JSON Schema differs from %s.\n"+
			"If the change is intended, run `go test ./schema -update` and review the diff.\n"+
			"--- got ---\n%s", goldenPath, got)
	}
}

// JSONSchema is memoized; it must still be a correct, stable value.
func TestJSONSchemaIsStableAndWellFormed(t *testing.T) {
	first := JSONSchema()
	if JSONSchema() != first {
		t.Fatal("JSONSchema() is not stable across calls")
	}
	if !strings.HasSuffix(first, "\n") {
		t.Error("JSONSchema() must end in a newline so it can be redirected to a file as-is")
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(first), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if doc["$schema"] != jsonSchemaDialect {
		t.Errorf("$schema = %v, want %v", doc["$schema"], jsonSchemaDialect)
	}
	if doc["$id"] != SchemaID {
		t.Errorf("$id = %v, want %v", doc["$id"], SchemaID)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("no $defs")
	}
	for _, o := range v1Objects {
		if _, ok := defs[o.name]; !ok {
			t.Errorf("$defs is missing %s", o.name)
		}
	}
}

// Every $ref must resolve. A typo in a descriptor's ref/itemRef would
// otherwise emit a schema that silently validates nothing at that position.
func TestEveryRefResolves(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(JSONSchema()), &doc); err != nil {
		t.Fatal(err)
	}
	defs, _ := doc["$defs"].(map[string]any)

	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if r, ok := v["$ref"].(string); ok {
				name, found := strings.CutPrefix(r, "#/$defs/")
				if !found {
					t.Errorf("unexpected $ref form %q", r)
				} else if _, ok := defs[name]; !ok {
					t.Errorf("$ref %q does not resolve", r)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

// The vocabularies must be advertised as OPEN. Emitting `enum` would make a
// conforming future v1 document — one carrying a status or outcome added
// later — fail validation against this schema, contradicting the
// compatibility rule the package documents.
func TestOpenVocabulariesUseExamplesNotEnum(t *testing.T) {
	if strings.Contains(JSONSchema(), `"enum"`) {
		t.Error("the schema emits `enum`; v1 vocabularies are open, so use examples instead")
	}
	for _, want := range []string{StatusPending, OutcomePending, ErrKindTimeout} {
		if !strings.Contains(JSONSchema(), `"`+want+`"`) {
			t.Errorf("known value %q is not advertised anywhere in the schema", want)
		}
	}
	// additionalProperties must stay permitted, for the same reason: v1 may
	// gain properties.
	if strings.Contains(JSONSchema(), "additionalProperties") {
		t.Error("the schema constrains additionalProperties; v1 may gain properties")
	}
}

// required must mirror omitempty exactly, because that is precisely when the
// encoder always writes the property. The one that matters is minion.retcode:
// required AND nullable, so a consumer finds null rather than a missing key.
func TestRequiredMirrorsOmitempty(t *testing.T) {
	var doc struct {
		Defs map[string]struct {
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(JSONSchema()), &doc); err != nil {
		t.Fatal(err)
	}
	for _, o := range v1Objects {
		def := doc.Defs[o.name]
		req := map[string]bool{}
		for _, r := range def.Required {
			req[r] = true
		}
		for _, f := range o.fields {
			if got, want := req[f.name()], f.required(); got != want {
				t.Errorf("%s.%s required=%v, want %v (tag %q)", o.name, f.name(), got, want, f.tag)
			}
		}
	}
	rc := doc.Defs["Minion"].Properties["retcode"]
	types, _ := rc["type"].([]any)
	if len(types) != 2 || types[0] != "integer" || types[1] != "null" {
		t.Errorf(`minion.retcode type = %v, want ["integer","null"]`, rc["type"])
	}
}

// A document the package itself produces must satisfy the schema's own
// required/const claims.
//
// This is a structural check, not a validator — papurika has no JSON Schema
// implementation as a dependency and does not need one. The committed golden
// in testdata was checked against a real Draft 2020-12 validator by hand, for
// every output shape (exec, failing exec, highstate, runner, wheel, no-match,
// client- and server-batched, grains, a pending job, and both error
// envelopes); redo that with check-jsonschema if the schema changes shape.
func TestProducedDocumentsSatisfyRequiredProperties(t *testing.T) {
	docs := map[string]Result{
		"full":  FromResult(sampleResult()),
		"empty": FromResult(&papurika.Result{}),
		"error": FromError(fmt.Errorf("boom")),
	}
	var js struct {
		Defs map[string]struct {
			Required []string `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(JSONSchema()), &js); err != nil {
		t.Fatal(err)
	}
	for name, doc := range docs {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, r := range js.Defs["Result"].Required {
			if _, ok := m[r]; !ok {
				t.Errorf("%s document is missing required property %q: %s", name, r, b)
			}
		}
		if string(m["schema"]) != `"`+Version+`"` {
			t.Errorf("%s document schema = %s, want %q", name, m["schema"], Version)
		}
	}
}
