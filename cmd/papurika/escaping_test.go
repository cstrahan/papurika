package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cstrahan/papurika"
	sigsyaml "sigs.k8s.io/yaml"
)

func yamlToJSON(b []byte, into any) error {
	j, err := sigsyaml.YAMLToJSON(b)
	if err != nil {
		return err
	}
	return json.Unmarshal(j, into)
}

// payload is the kind of thing cmd.run actually returns: shell redirects,
// markup, an ampersand. encoding/json escapes all three by default.
const payload = `<config a="1" & b='2'> 2>&1`

func withPayload() *papurika.Result {
	// Built WITHOUT json.Marshal on purpose: that escapes, and the escapes
	// would then be baked into the RawMessage before any encoder saw it.
	// Salt sends literal <, > and & (Python's json.dumps does not escape
	// them) and papurika keeps minion returns verbatim, so this is what the
	// renderer really receives.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	raw := json.RawMessage(bytes.TrimSpace(b.Bytes()))
	return &papurika.Result{Commands: []papurika.CommandResult{{
		Client: papurika.ClientLocal, Fun: "cmd.run", JID: "1",
		Targeted: []string{"a"},
		Minions: map[string]papurika.MinionResult{
			"a": {ID: "a", Status: papurika.StatusResponded, Retcode: intp(0),
				Value: papurika.NewRawValue(raw)},
		},
	}}}
}

// stdout is for people and for jq, not for a <script> block. Escaping <, > and
// & turns the payloads most worth reading — command output with redirects,
// XML, HTML, SQL — into < noise.
func TestJSONOutputDoesNotHTMLEscape(t *testing.T) {
	for _, mode := range []outputMode{outputJSON, outputNDJSON} {
		var buf bytes.Buffer
		if err := render(&buf, withPayload(), mode, renderOpts{}); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
			if strings.Contains(got, esc) {
				t.Errorf("%s output HTML-escapes %s:\n%s", mode, esc, got)
			}
		}
		// The characters themselves must survive, only JSON's own quote
		// escaping remaining.
		if !strings.Contains(got, `<config a=\"1\" & b='2'> 2>&1`) {
			t.Errorf("%s output lost the payload:\n%s", mode, got)
		}
	}
}

// json and yaml are the same model in two spellings, so they must not
// disagree about the same bytes. They did: the YAML path decodes the escapes
// on its way through, so json said < where yaml said <.
func TestJSONAndYAMLAgreeOnPayload(t *testing.T) {
	var j, y bytes.Buffer
	if err := render(&j, withPayload(), outputJSON, renderOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := render(&y, withPayload(), outputYAML, renderOpts{}); err != nil {
		t.Fatal(err)
	}
	// Both must round-trip to the identical value.
	var jd, yd map[string]any
	if err := json.Unmarshal(j.Bytes(), &jd); err != nil {
		t.Fatal(err)
	}
	if err := yamlToJSON(y.Bytes(), &yd); err != nil {
		t.Fatal(err)
	}
	jv := jd["commands"].([]any)[0].(map[string]any)["minions"].([]any)[0].(map[string]any)["return"]
	yv := yd["commands"].([]any)[0].(map[string]any)["minions"].([]any)[0].(map[string]any)["return"]
	if jv != payload || yv != payload {
		t.Errorf("json=%q yaml=%q want %q", jv, yv, payload)
	}
	// And neither spelling may carry an escape the other does not.
	if strings.Contains(j.String(), `\u00`) {
		t.Errorf("json carries escapes yaml does not:\n%s", j.String())
	}
}
