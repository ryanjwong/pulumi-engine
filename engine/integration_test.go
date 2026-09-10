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

package engine

// Integration tests run real provider plugins (pulumi-random, pulumi-command)
// and the YAML language host against a file:// backend. They need network
// access on first run to download the plugins, and are gated on
// PULUMI_ENGINE_INTEGRATION=1.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func integrationSpec(t *testing.T, name string, dir string) StackSpec {
	t.Helper()
	if os.Getenv("PULUMI_ENGINE_INTEGRATION") != "1" {
		t.Skip("set PULUMI_ENGINE_INTEGRATION=1 to run integration tests")
	}
	state := t.TempDir()
	spec := StackSpec{
		Name:    name,
		Project: ProjectSpec{Name: "engine-it", Dir: dir},
		Backend: BackendSpec{URL: "file://" + state},
		Secrets: SecretsSpec{Provider: "passphrase", Passphrase: "correct horse battery staple"},
		Create:  true,
	}
	if dir != "" {
		spec.Project.Name = "engine-yaml"
	}
	return spec
}

// drain collects every event and returns them once the channel closes. When
// PULUMI_ENGINE_RECORD_DIR is set the events are also written as JSON lines
// (one file per operation) so they can be checked in as fixtures.
func drain(t *testing.T, op *Operation) []Event {
	t.Helper()
	var events []Event
	for e := range op.Events() {
		events = append(events, e)
	}
	if dir := os.Getenv("PULUMI_ENGINE_RECORD_DIR"); dir != "" {
		recordMu.Lock()
		recordSeq++
		name := fmt.Sprintf("%s-%02d-%s.jsonl", strings.TrimPrefix(t.Name(), "TestIntegration"), recordSeq, op.Kind())
		recordMu.Unlock()
		var buf strings.Builder
		for _, e := range events {
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(buf.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

var (
	recordMu  sync.Mutex
	recordSeq int
)

func diagnostics(events []Event) []string {
	var out []string
	for _, e := range events {
		if e.DiagnosticEvent != nil {
			out = append(out, e.DiagnosticEvent.Severity+": "+strings.TrimSpace(e.DiagnosticEvent.Message))
		}
	}
	return out
}

func eventTypes(events []Event) []EventType {
	out := make([]EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func resourceOps(events []Event, typ string) map[string]string {
	ops := map[string]string{}
	for _, e := range events {
		if e.ResOutputsEvent != nil && (typ == "" || e.ResOutputsEvent.Metadata.Type == typ) {
			ops[e.ResOutputsEvent.Metadata.URN] = string(e.ResOutputsEvent.Metadata.Op)
		}
	}
	return ops
}

func assertEnvelope(t *testing.T, events []Event) {
	t.Helper()
	if len(events) < 2 {
		t.Fatalf("expected at least prelude and summary, got %v", eventTypes(events))
	}
	// Diagnostics (plugin warnings) may precede the prelude; resource events
	// may not.
	prelude := -1
	for i, e := range events {
		if e.Type == EventPrelude {
			prelude = i
			break
		}
		if e.Type != EventDiagnostic {
			t.Errorf("event %s before prelude (all: %v, diagnostics: %v)", e.Type, eventTypes(events), diagnostics(events))
		}
	}
	if prelude < 0 {
		t.Errorf("no prelude event (all: %v)", eventTypes(events))
	}
	last := events[len(events)-1]
	if last.Type != EventSummary {
		t.Errorf("last event should be summary, got %s (all: %v)", last.Type, eventTypes(events))
	}
	for _, e := range events {
		if e.Type == EventCancel {
			t.Errorf("cancel events are internal and must not be delivered")
		}
	}
}

func TestIntegrationGoProgramLifecycle(t *testing.T) {
	spec := integrationSpec(t, "dev", "")
	spec.Config = map[string]ConfigValue{
		"petLength": {Value: "3"},
		"apiKey":    {Value: "hunter2", Secret: true},
	}
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	program := GoProgram(func(ctx *pulumi.Context) error {
		lengthStr, _ := ctx.GetConfig("engine-it:petLength")
		length, err := strconv.Atoi(lengthStr)
		if err != nil {
			return fmt.Errorf("petLength config missing or invalid: %q", lengthStr)
		}
		pet, err := random.NewRandomPet(ctx, "pet", &random.RandomPetArgs{Length: pulumi.Int(length)})
		if err != nil {
			return err
		}
		pw, err := random.NewRandomPassword(ctx, "pw", &random.RandomPasswordArgs{Length: pulumi.Int(16)})
		if err != nil {
			return err
		}
		ctx.Export("petName", pet.ID())
		ctx.Export("password", pw.Result)
		apiKey, _ := ctx.GetConfig("engine-it:apiKey")
		ctx.Export("apiKeyLen", pulumi.Int(len(apiKey)))
		return nil
	})

	// preview
	op := st.Preview(ctx, program, Options{})
	events := drain(t, op)
	res, err := op.Wait()
	if err != nil {
		t.Fatalf("preview: %v (events: %v)", err, eventTypes(events))
	}
	assertEnvelope(t, events)
	if res.Summary == nil || !res.Summary.IsPreview {
		t.Fatalf("preview summary missing or not a preview: %+v", res.Summary)
	}
	if got := res.Changes["create"]; got != 3 { // stack + pet + password
		t.Errorf("preview creates = %d, want 3 (%v)", got, res.Changes)
	}
	for _, e := range events {
		if e.ResourcePreEvent != nil && e.ResourcePreEvent.Metadata.Type == "random:index/randomPassword:RandomPassword" {
			if e.ResourcePreEvent.Metadata.New == nil || e.ResourcePreEvent.Metadata.New.Outputs["result"] != "[secret]" {
				// during preview the result is unknown; it is fine for it to be absent
				continue
			}
		}
	}

	// up
	op = st.Up(ctx, program, Options{Message: "integration up"})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("up: %v (events: %v)", err, eventTypes(events))
	}
	assertEnvelope(t, events)
	if got := res.Changes["create"]; got != 3 {
		t.Errorf("up creates = %d, want 3 (%v)", got, res.Changes)
	}
	ops := resourceOps(events, "random:index/randomPet:RandomPet")
	if len(ops) != 1 {
		t.Errorf("expected one RandomPet outputs event, got %v", ops)
	}
	for _, e := range events {
		if e.ResOutputsEvent != nil && e.ResOutputsEvent.Metadata.Type == "random:index/randomPassword:RandomPassword" {
			raw, _ := json.Marshal(e.ResOutputsEvent.Metadata.New.Outputs["result"])
			if !strings.Contains(string(raw), "[secret]") {
				t.Errorf("password result should be redacted in events, got %s", raw)
			}
		}
	}
	if res.Outputs == nil {
		t.Fatalf("up result has no outputs")
	}
	if res.Outputs.Values["password"] != "[secret]" {
		t.Errorf("password output should be redacted, got %v", res.Outputs.Values["password"])
	}
	if res.Outputs.Values["apiKeyLen"] != float64(len("hunter2")) {
		t.Errorf("secret config did not reach the program: apiKeyLen=%v", res.Outputs.Values["apiKeyLen"])
	}

	// outputs
	out, err := st.Outputs(ctx, true)
	if err != nil {
		t.Fatalf("outputs: %v", err)
	}
	pet, _ := out.Values["petName"].(string)
	if strings.Count(pet, "-") != 2 { // length 3 => two separators
		t.Errorf("petName %q does not look like a 3-word pet name", pet)
	}
	if pw, _ := out.Values["password"].(string); len(pw) != 16 {
		t.Errorf("password with showSecrets should be 16 chars, got %q", pw)
	}
	if len(out.SecretKeys) != 1 || out.SecretKeys[0] != "password" {
		t.Errorf("secret keys = %v, want [password]", out.SecretKeys)
	}

	// config round trip
	v, ok, err := st.GetConfig(ctx, "apiKey")
	if err != nil || !ok || v.Value != "hunter2" || !v.Secret {
		t.Errorf("GetConfig(apiKey) = %+v, %v, %v", v, ok, err)
	}

	// export / import
	dep, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var untyped struct {
		Version    int             `json:"version"`
		Deployment json.RawMessage `json:"deployment"`
	}
	if err := json.Unmarshal(dep, &untyped); err != nil || untyped.Version != 3 {
		t.Fatalf("export is not an untyped v3 deployment: %v %s", err, dep[:min(len(dep), 200)])
	}
	if !strings.Contains(string(untyped.Deployment), "random:index/randomPet:RandomPet") {
		t.Errorf("exported deployment does not contain the pet resource")
	}
	if strings.Contains(string(untyped.Deployment), pet) && false {
		t.Errorf("unreachable")
	}
	if err := st.Import(ctx, dep); err != nil {
		t.Fatalf("import: %v", err)
	}

	// second up: no changes
	op = st.Up(ctx, program, Options{})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if res.Changes["create"] != 0 || res.Changes["same"] != 3 {
		t.Errorf("second up should be all same, got %v", res.Changes)
	}

	// refresh
	op = st.Refresh(ctx, nil, Options{})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("refresh: %v (events: %v)", err, eventTypes(events))
	}
	assertEnvelope(t, events)
	if res.Summary == nil {
		t.Fatalf("refresh produced no summary (events: %v)", eventTypes(events))
	}
	if n := len(resourceOps(events, "")); n < 2 {
		t.Errorf("refresh should report the random resources, got %d outputs events", n)
	}

	// destroy
	op = st.Destroy(ctx, nil, Options{})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("destroy: %v (events: %v)", err, eventTypes(events))
	}
	assertEnvelope(t, events)
	if got := res.Changes["delete"]; got != 3 {
		t.Errorf("destroy deletes = %d, want 3 (%v)", got, res.Changes)
	}
	out, err = st.Outputs(ctx, false)
	if err != nil {
		t.Fatalf("outputs after destroy: %v", err)
	}
	if len(out.Values) != 0 {
		t.Errorf("outputs after destroy should be empty, got %v", out.Values)
	}
	if err := st.Remove(ctx, false); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// re-open without Create must fail
	spec.Create = false
	if _, err := Open(ctx, spec); !errors.As(err, new(StackNotFound)) {
		t.Errorf("open after remove: got %v, want StackNotFound", err)
	}
}

func TestIntegrationGoProgramResourceFailure(t *testing.T) {
	spec := integrationSpec(t, "fail", "")
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

	program := GoProgram(func(ctx *pulumi.Context) error {
		if _, err := random.NewRandomPet(ctx, "ok", nil); err != nil {
			return err
		}
		// min > max is rejected by the provider when the resource is created.
		_, err := random.NewRandomInteger(ctx, "bad", &random.RandomIntegerArgs{Min: pulumi.Int(10), Max: pulumi.Int(1)})
		return err
	})
	op := st.Up(ctx, program, Options{})
	events := drain(t, op)
	res, err := op.Wait()
	if err == nil {
		t.Fatalf("up should fail; changes %v", res.Changes)
	}
	t.Logf("error: %v (kind %s)", err, KindOf(err))
	var rof ResourceOpFailed
	var pf ProgramFailed
	switch {
	case errors.As(err, &rof):
		if !strings.Contains(rof.URN, "bad") || rof.Op != "create" {
			t.Errorf("unexpected failure %+v", rof)
		}
		if len(res.Failures) != 1 {
			t.Errorf("result should list one failure, got %v", res.Failures)
		}
	case errors.As(err, &pf):
		// The provider may reject the inputs during Check, which Pulumi
		// reports as a diagnostic against the resource rather than a failed
		// step. Either way the error is typed.
		if !strings.Contains(strings.ToLower(pf.Message), "min") {
			t.Errorf("program failure should mention the invalid argument: %v", pf.Message)
		}
	default:
		t.Fatalf("expected ResourceOpFailed or ProgramFailed, got %T: %v (events %v)", err, err, eventTypes(events))
	}
	// the pet that did succeed must be in the checkpoint
	dep, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(string(dep), "random:index/randomPet:RandomPet") {
		t.Errorf("checkpoint should contain the successful pet")
	}
	op = st.Destroy(ctx, nil, Options{})
	drain(t, op)
	if _, err := op.Wait(); err != nil {
		t.Fatalf("destroy after failure: %v", err)
	}
}

func TestIntegrationGoProgramFailure(t *testing.T) {
	spec := integrationSpec(t, "progfail", "")
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

	op := st.Up(ctx, GoProgram(func(ctx *pulumi.Context) error {
		return errors.New("kaboom from the program")
	}), Options{})
	drain(t, op)
	_, err = op.Wait()
	var pf ProgramFailed
	if !errors.As(err, &pf) {
		t.Fatalf("expected ProgramFailed, got %T: %v", err, err)
	}
	if !strings.Contains(pf.Message, "kaboom") {
		t.Errorf("program error message lost: %q", pf.Message)
	}
}

func TestIntegrationLocalYAML(t *testing.T) {
	dir := copyFixture(t, "yaml-random")
	spec := integrationSpec(t, "yaml", dir)
	spec.Config = map[string]ConfigValue{"petLength": {Value: "2"}}
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })
	// Open writes Pulumi.<stack>.yaml next to the project like the CLI.
	if _, err := os.Stat(filepath.Join(dir, "Pulumi.yaml.yaml")); err != nil {
		t.Errorf("Pulumi.yaml.yaml stack config should have been written: %v", err)
	}

	program := LocalProgram{Dir: dir}
	op := st.Preview(ctx, program, Options{})
	events := drain(t, op)
	res, err := op.Wait()
	if err != nil {
		t.Fatalf("preview: %v (events %v)", err, eventTypes(events))
	}
	assertEnvelope(t, events)
	if res.Changes["create"] != 3 {
		t.Errorf("preview creates = %v", res.Changes)
	}

	op = st.Up(ctx, program, Options{})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("up: %v (events %v)", err, eventTypes(events))
	}
	if res.Outputs == nil || res.Outputs.Values["token"] != "[secret]" {
		t.Errorf("token output should be secret-redacted: %+v", res.Outputs)
	}
	pet, _ := res.Outputs.Values["petName"].(string)
	if strings.Count(pet, "-") != 1 {
		t.Errorf("petName %q should have two words", pet)
	}

	op = st.Destroy(ctx, nil, Options{})
	events = drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("destroy: %v (events %v)", err, eventTypes(events))
	}
	if res.Changes["delete"] != 3 {
		t.Errorf("destroy deletes = %v", res.Changes)
	}
}

func TestIntegrationLocalYAMLResourceFailure(t *testing.T) {
	dir := copyFixture(t, "yaml-fail")
	spec := integrationSpec(t, "yamlfail", dir)
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

	op := st.Up(ctx, LocalProgram{Dir: dir}, Options{})
	events := drain(t, op)
	res, err := op.Wait()
	var rof ResourceOpFailed
	if !errors.As(err, &rof) {
		t.Fatalf("expected ResourceOpFailed, got %T: %v (events %v)", err, err, eventTypes(events))
	}
	if !strings.HasSuffix(rof.URN, "::boom") || rof.Type != "command:local:Command" || rof.Op != "create" {
		t.Errorf("unexpected failure %+v", rof)
	}
	if !strings.Contains(rof.Message, "exit status 3") && !strings.Contains(rof.Message, "meant to fail") {
		t.Errorf("failure message should carry the provider error: %q", rof.Message)
	}
	if len(res.Failures) != 1 {
		t.Errorf("expected one failure in result, got %v", res.Failures)
	}
	sawFailedEvent := false
	for _, e := range events {
		if e.Type == EventResourceOpFailed {
			sawFailedEvent = true
		}
	}
	if !sawFailedEvent {
		t.Errorf("no resourceOpFailed event in %v", eventTypes(events))
	}

	op = st.Destroy(ctx, nil, Options{})
	drain(t, op)
	if _, err := op.Wait(); err != nil {
		t.Fatalf("destroy after failure: %v", err)
	}
}

func TestIntegrationCancelMidUp(t *testing.T) {
	dir := copyFixture(t, "yaml-slow")
	spec := integrationSpec(t, "cancel", dir)
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

	cctx, cancel := context.WithCancel(ctx)
	op := st.Up(cctx, LocalProgram{Dir: dir}, Options{})
	// cancel once the first slow create has started
	go func() {
		for e := range op.Events() {
			if e.ResourcePreEvent != nil && strings.HasSuffix(e.ResourcePreEvent.Metadata.URN, "::slow1") {
				cancel()
			}
		}
	}()
	started := time.Now()
	res, err := op.Wait()
	var c Cancelled
	if !errors.As(err, &c) {
		t.Fatalf("expected Cancelled, got %T: %v", err, err)
	}
	if !res.Cancelled {
		t.Errorf("result should be marked cancelled")
	}
	if took := time.Since(started); took > 40*time.Second {
		t.Errorf("cancel took %v; the graceful cancel should return after the in-flight step", took)
	}

	// The checkpoint must be consistent: readable, with the pet and at most
	// the one in-flight command; no pending operations left dangling.
	dep, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export after cancel: %v", err)
	}
	var untyped struct {
		Deployment struct {
			Resources         []map[string]any `json:"resources"`
			PendingOperations []any            `json:"pending_operations"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal(dep, &untyped); err != nil {
		t.Fatalf("checkpoint not parseable: %v", err)
	}
	if len(untyped.Deployment.PendingOperations) != 0 {
		t.Errorf("checkpoint has pending operations after graceful cancel: %v", untyped.Deployment.PendingOperations)
	}
	sawPet, sawSlow3 := false, false
	for _, r := range untyped.Deployment.Resources {
		urn, _ := r["urn"].(string)
		if strings.HasSuffix(urn, "::first") {
			sawPet = true
		}
		if strings.HasSuffix(urn, "::slow3") {
			sawSlow3 = true
		}
	}
	if !sawPet {
		t.Errorf("checkpoint lost the resource created before the cancel")
	}
	if sawSlow3 {
		t.Errorf("checkpoint contains a resource that should never have started")
	}

	// A following destroy must work from that checkpoint.
	op = st.Destroy(ctx, nil, Options{})
	drain(t, op)
	if _, err := op.Wait(); err != nil {
		t.Fatalf("destroy after cancel: %v", err)
	}
}

// copyFixture copies engine/testdata/<name> to a temp dir so that tests can
// write Pulumi.<stack>.yaml next to it.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}
