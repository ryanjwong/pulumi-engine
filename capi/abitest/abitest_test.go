// Copyright 2026 Ryan Wong
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build abitest

package abitest

// Smoke test of the C ABI: open -> op -> events -> wait -> free, offline
// (file backend, no providers). The test binary carries its own Go runtime
// next to the one inside the shared library; the in-process program server
// it starts talks to the library's engine over loopback gRPC, like any
// callback program would.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ryanjwong/pulumi-engine/engine"
)

func decodeErr(t *testing.T, errJSON string) map[string]any {
	t.Helper()
	if errJSON == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(errJSON), &m); err != nil {
		t.Fatalf("error is not JSON: %v: %s", err, errJSON)
	}
	return m
}

func drainEvents(t *testing.T, id int64) (types []string, timeouts int) {
	t.Helper()
	for {
		s, end := OpNextEvent(id, 50)
		if end {
			return types, timeouts
		}
		if s == "" {
			timeouts++
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(s), &ev); err != nil {
			t.Fatalf("event is not JSON: %v: %s", err, s)
		}
		types = append(types, ev.Type)
	}
}

func TestABISmoke(t *testing.T) {
	if v := Version(); !strings.Contains(v, "pulumi-engine") {
		t.Errorf("version = %q", v)
	}

	spec, _ := json.Marshal(map[string]any{
		"name":    "dev",
		"project": map[string]any{"name": "abi", "dir": t.TempDir()},
		"backend": map[string]any{"url": "file://" + t.TempDir()},
		"secrets": map[string]any{"provider": "b64"},
		"config":  map[string]any{"greeting": map[string]any{"value": "hi"}},
		"create":  true,
	})
	h, e := StackOpen(string(spec))
	if h == 0 {
		t.Fatalf("open failed: %v", decodeErr(t, e))
	}

	srv, err := engine.ServeGoProgram(func(ctx *pulumi.Context) error {
		g, _ := ctx.GetConfig("abi:greeting")
		ctx.Export("greeting", pulumi.String(g))
		ctx.Export("secret", pulumi.ToSecret(pulumi.String("shh")))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id, e := OpStart(h, `{"kind":"up","program":{"mode":"callback","address":"`+srv.Address()+`"},"options":{"message":"abi"}}`)
	if id == 0 {
		t.Fatalf("op_start failed: %v", decodeErr(t, e))
	}
	types, timeouts := drainEvents(t, id)
	if len(types) == 0 || types[len(types)-1] != "summary" {
		t.Errorf("events %v", types)
	}
	t.Logf("events %v (timeouts %d)", types, timeouts)
	resJSON, e := OpWait(id)
	if resJSON == "" {
		t.Fatalf("wait failed: %v", decodeErr(t, e))
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(resJSON), &res); err != nil {
		t.Fatal(err)
	}
	if res["kind"] != "up" || res["outputs"].(map[string]any)["values"].(map[string]any)["greeting"] != "hi" {
		t.Errorf("result %v", res)
	}
	if _, ok := res["durationMs"]; !ok {
		t.Errorf("result lacks durationMs: %v", res)
	}
	if OpRelease(id) != 0 || OpRelease(id) != -1 {
		t.Errorf("release semantics")
	}
	if _, end := OpNextEvent(id, 0); !end {
		t.Errorf("released op should be at end")
	}
	_ = srv.Close()

	out, e := StackOutputs(h, false)
	if !strings.Contains(out, `"secret":"[secret]"`) || !strings.Contains(out, `"secretKeys":["secret"]`) {
		t.Errorf("outputs %s (%v)", out, decodeErr(t, e))
	}
	if out, _ := StackOutputs(h, true); !strings.Contains(out, `"secret":"shh"`) {
		t.Errorf("showSecrets outputs %s", out)
	}

	if rc, e := StackSetConfig(h, "token", `{"value":"s3cret","secret":true}`); rc != 0 {
		t.Fatalf("set_config: %v", decodeErr(t, e))
	}
	if got, _ := StackGetConfig(h, "token"); got != `{"value":"s3cret","secret":true}` {
		t.Errorf("get_config = %s", got)
	}
	if got, _ := StackGetConfig(h, "missing"); got != "null" {
		t.Errorf("missing config = %s", got)
	}

	dep, e := StackExport(h)
	if !strings.Contains(dep, `"version":3`) {
		t.Errorf("export %.100s (%v)", dep, decodeErr(t, e))
	}
	if rc, e := StackImport(h, dep); rc != 0 {
		t.Fatalf("import: %v", decodeErr(t, e))
	}
	if rc, e := StackImport(h, `{"nope":1}`); rc != -1 || decodeErr(t, e)["kind"] != "invalidSpec" {
		t.Errorf("bad import: rc=%d err=%s", rc, e)
	}

	failing, err := engine.ServeGoProgram(func(*pulumi.Context) error { panic("abi boom") })
	if err != nil {
		t.Fatal(err)
	}
	id, e = OpStart(h, `{"kind":"up","program":{"mode":"callback","address":"`+failing.Address()+`"}}`)
	if id == 0 {
		t.Fatalf("op_start: %v", decodeErr(t, e))
	}
	drainEvents(t, id)
	resJSON, e = OpWait(id)
	if resJSON != "" {
		t.Fatalf("failing program should fail: %s", resJSON)
	}
	em := decodeErr(t, e)
	if em["kind"] != "programFailed" || !strings.Contains(em["message"].(string), "abi boom") {
		t.Errorf("error %v", em)
	}
	if _, ok := em["result"].(map[string]any); !ok {
		t.Errorf("error should carry the partial result: %v", em)
	}
	OpRelease(id)
	_ = failing.Close()
	if OpCancel(id) != -1 {
		t.Errorf("cancel on released op should return -1")
	}

	id, e = OpStart(h, `{"kind":"destroy"}`)
	if id == 0 {
		t.Fatalf("destroy start: %v", decodeErr(t, e))
	}
	drainEvents(t, id)
	if resJSON, e = OpWait(id); resJSON == "" {
		t.Fatalf("destroy: %v", decodeErr(t, e))
	}
	OpRelease(id)
	if rc, e := StackRemove(h, false); rc != 0 {
		t.Fatalf("remove: %v", decodeErr(t, e))
	}
	if StackClose(h) != 0 || StackClose(h) != -1 {
		t.Errorf("close semantics")
	}
	if _, e := StackExport(h); decodeErr(t, e)["kind"] != "invalidSpec" {
		t.Errorf("closed handle error %s", e)
	}
	if id, e := OpStart(999999, `{"kind":"up","program":{"mode":"weird"}}`); id != 0 || e == "" {
		t.Errorf("unknown stack should fail")
	}
}

func TestABIInvalidSpec(t *testing.T) {
	h, e := StackOpen(`{"name":"dev"}`)
	if h != 0 {
		t.Fatalf("open should fail")
	}
	if em := decodeErr(t, e); em["kind"] != "invalidSpec" || em["field"] != "backend.url" {
		t.Errorf("error %v", em)
	}
	if h, e := StackOpen(`nope`); h != 0 || decodeErr(t, e)["kind"] != "invalidSpec" {
		t.Errorf("non-JSON spec: %d %s", h, e)
	}
	if StackOpenNoErr(`nope`) != 0 {
		t.Errorf("NULL err out-parameter must be accepted")
	}
}
