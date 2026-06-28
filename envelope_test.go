package papurika

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeMinionValue(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantStatus Status
		wantRet    *int // nil = expect no retcode
		wantVal    string
	}{
		{"ping true", `true`, StatusResponded, nil, "true"},
		{"cmd.run string", `"hello\nworld"`, StatusResponded, nil, `"hello\nworld"`},
		{"no response", `"Minion did not return. [No response]"`, StatusNoResponse, nil, ""},
		{"unknown fun", `"'does.not.exist' is not available."`, StatusError, nil, ""},
		{"full_return wrapper", `{"ret":true,"retcode":0,"jid":"123"}`, StatusResponded, intPtr(0), "true"},
		{"full_return nonzero", `{"ret":"","retcode":3,"jid":"123"}`, StatusResponded, intPtr(3), `""`},
		{"batch splice (state)", `{"retcode":2,"state_x":{"result":false}}`, StatusResponded, intPtr(2), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := decodeMinionValue("m", json.RawMessage(tc.raw))
			if mr.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", mr.Status, tc.wantStatus)
			}
			switch {
			case tc.wantRet == nil && mr.Retcode != nil:
				t.Errorf("retcode = %d, want nil", *mr.Retcode)
			case tc.wantRet != nil && mr.Retcode == nil:
				t.Errorf("retcode = nil, want %d", *tc.wantRet)
			case tc.wantRet != nil && *mr.Retcode != *tc.wantRet:
				t.Errorf("retcode = %d, want %d", *mr.Retcode, *tc.wantRet)
			}
			if tc.wantVal != "" {
				if got := string(mr.Value.Raw()); got != tc.wantVal {
					t.Errorf("value = %s, want %s", got, tc.wantVal)
				}
			}
		})
	}
}

func TestDecodeExecResponse_LocalAggregate(t *testing.T) {
	body := `{"return":[{"a":true,"b":true}]}`
	res, err := decodeExecResponse([]Command{Local("*", "test.ping")}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commands) != 1 {
		t.Fatalf("got %d commands", len(res.Commands))
	}
	if len(res.Commands[0].Minions) != 2 {
		t.Fatalf("got %d minions, want 2", len(res.Commands[0].Minions))
	}
	if got := res.Responders(); len(got) != 2 {
		t.Errorf("responders = %v, want 2", got)
	}
}

func TestDecodeExecResponse_BatchFanoutMerges(t *testing.T) {
	// local_batch returns one entry per chunk; a single command's entries must
	// merge into one minion map.
	body := `{"return":[{"b":"x"},{"a":"y"}]}`
	res, err := decodeExecResponse([]Command{{Client: ClientLocalBatch, Tgt: "*", Fun: "cmd.run"}}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commands) != 1 {
		t.Fatalf("got %d commands, want 1 (batch fan-out must merge)", len(res.Commands))
	}
	if len(res.Commands[0].Minions) != 2 {
		t.Errorf("got %d minions, want 2", len(res.Commands[0].Minions))
	}
}

func TestDecodeExecResponse_Async(t *testing.T) {
	body := `{"return":[{"jid":"20260626233617769051","minions":["m1","m2"]}]}`
	res, err := decodeExecResponse([]Command{{Client: ClientLocalAsync, Tgt: "*", Fun: "x"}}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	c := res.Commands[0]
	if c.JID != "20260626233617769051" {
		t.Errorf("jid = %q", c.JID)
	}
	if len(c.Targeted) != 2 {
		t.Errorf("targeted = %v, want 2", c.Targeted)
	}
}

func TestDecodeExecResponse_WheelUnwrap(t *testing.T) {
	body := `{"return":[{"tag":"salt/wheel/1","data":{"fun":"wheel.key.list_all","success":true,"return":{"minions":["m1"]}}}]}`
	res, err := decodeExecResponse([]Command{Wheel("key.list_all")}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	c := res.Commands[0]
	if c.Success == nil || !*c.Success {
		t.Errorf("success = %v, want true", c.Success)
	}
	m, ok := c.Value.Map()
	if !ok || m["minions"] == nil {
		t.Errorf("value did not unwrap data.return: %s", c.Value.Raw())
	}
}

func TestDecodeExecResponse_RunnerFullReturn(t *testing.T) {
	body := `{"return":[{"tag":"salt/run/1/ret","data":{"fun":"runner.manage.status","success":true,"return":{"up":["m1"],"down":["m2"]}}}]}`
	res, err := decodeExecResponse([]Command{Runner("manage.status")}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.Commands[0].Value.Map()
	if !ok || m["up"] == nil || m["down"] == nil {
		t.Errorf("runner value not unwrapped: %s", res.Commands[0].Value.Raw())
	}
}

// Smoke test: real captured responses decode without panic and yield the
// expected minion ids. These captures predate full_return, exercising the
// bare-value paths.
func TestDecodeAgainstCaptures(t *testing.T) {
	dir := filepath.Join("research", "salt-test-cherrypy", "out")
	cases := []struct {
		file    string
		cmd     Command
		wantMin []string // expected minion ids (subset check)
	}{
		{"17a_aggregate_test_ping.json", Local("*", "test.ping"), []string{"builtin.minion", "builtin.minion2"}},
		{"17b_local_batch_two_minions.json", Command{Client: ClientLocalBatch, Tgt: "*", Fun: "cmd.run"}, []string{"builtin.minion", "builtin.minion2"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, tc.file))
			if err != nil {
				t.Skipf("capture not available: %v", err)
			}
			var cap struct {
				ResponseBody json.RawMessage `json:"response_body"`
			}
			if err := json.Unmarshal(raw, &cap); err != nil {
				t.Fatal(err)
			}
			res, err := decodeExecResponse([]Command{tc.cmd}, cap.ResponseBody)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range tc.wantMin {
				if _, ok := res.Commands[0].Minions[id]; !ok {
					t.Errorf("missing minion %q in decoded result", id)
				}
			}
		})
	}
}
