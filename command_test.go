package papurika

import (
	"encoding/json"
	"testing"
)

// Runners take positional args — `salt-run salt.cmd test.version` — and
// salt-api accepts `arg` for them. Dropping them produced salt's "Passed
// invalid arguments" usage text where a result was expected, which is
// especially nasty because salt still reports success:true for it.
func TestRunnerLowstateCarriesPositionalArgs(t *testing.T) {
	low := Runner("salt.cmd").WithArg("test.version").lowstate(true)

	args, ok := low["arg"].([]any)
	if !ok || len(args) != 1 || args[0] != "test.version" {
		t.Fatalf("lowstate arg = %#v, want [test.version]", low["arg"])
	}
	if low["fun"] != "salt.cmd" || low["client"] != "runner" {
		t.Errorf("lowstate = %#v", low)
	}
	// Runners act on the master and must still carry no target.
	if _, ok := low["tgt"]; ok {
		t.Errorf("runner lowstate must not carry tgt: %#v", low)
	}
}

func TestWheelLowstateCarriesPositionalArgs(t *testing.T) {
	low := Wheel("key.finger").WithArg("minion1").lowstate(true)
	if args, ok := low["arg"].([]any); !ok || len(args) != 1 {
		t.Errorf("wheel lowstate arg = %#v, want [minion1]", low["arg"])
	}
}

// A numeric enum in JSON tells a consumer nothing.
func TestModuleMarshalsByName(t *testing.T) {
	for m, want := range map[Module]string{
		ModuleCherryPy: `"rest_cherrypy"`,
		ModuleTornado:  `"rest_tornado"`,
		ModuleUnknown:  `"unknown"`,
	} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("Module(%d) marshals to %s, want %s", int(m), b, want)
		}
	}
}
