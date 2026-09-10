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

// CLI parity tests: the same program is run under this library and under
// the real `pulumi` CLI against the same file:// backend with the same
// passphrase, and the two are required to agree on state, secrets, config,
// events, locks and cancellation. See docs/cli-parity.md for the results.
//
// Gated on PULUMI_ENGINE_INTEGRATION=1 and on a `pulumi` binary
// (PULUMI_ENGINE_CLI overrides the one found on PATH).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const parityPassphrase = "correct horse battery staple"

// parityCLI is a pulumi CLI bound to one file:// backend and one passphrase.
type parityCLI struct {
	bin     string
	version string
	state   string // backend directory
	// extraEnv is appended to every CLI invocation (KEY=VALUE).
	extraEnv []string
}

// newParityCLI skips the test unless integration tests are enabled and a
// pulumi binary is available.
func newParityCLI(t *testing.T) *parityCLI {
	t.Helper()
	if os.Getenv("PULUMI_ENGINE_INTEGRATION") != "1" {
		t.Skip("set PULUMI_ENGINE_INTEGRATION=1 to run integration tests")
	}
	bin := os.Getenv("PULUMI_ENGINE_CLI")
	if bin == "" {
		p, err := exec.LookPath("pulumi")
		if err != nil {
			t.Skip("pulumi CLI not on PATH (set PULUMI_ENGINE_CLI to point at one)")
		}
		bin = p
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	c := &parityCLI{bin: abs, state: t.TempDir()}
	out, err := exec.Command(abs, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", abs, err)
	}
	c.version = strings.TrimSpace(string(out))
	t.Logf("CLI %s (%s); library %s", c.version, abs, Version())
	return c
}

func (c *parityCLI) backendURL() string { return "file://" + c.state }

// cmd builds a CLI invocation in dir. The environment is the test process
// environment (so PULUMI_HOME and the plugin cache are shared with the
// library) plus the backend URL and passphrase; nothing else is configured.
func (c *parityCLI) cmd(dir string, args ...string) *exec.Cmd {
	args = append(args, "--non-interactive")
	cmd := exec.Command(c.bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PULUMI_BACKEND_URL="+c.backendURL(),
		"PULUMI_CONFIG_PASSPHRASE="+parityPassphrase,
		"PULUMI_SKIP_UPDATE_CHECK=true",
		"PULUMI_DEBUG_COMMANDS=true", // unhides --event-log
		"PULUMI_SKIP_CONFIRMATIONS=true",
		"NO_COLOR=1",
	)
	cmd.Env = append(cmd.Env, c.extraEnv...)
	return cmd
}

// run executes a CLI command and returns stdout; stderr is folded into the
// error.
func (c *parityCLI) run(dir string, args ...string) (string, error) {
	cmd := c.cmd(dir, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("pulumi %s: %w\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func (c *parityCLI) mustRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := c.run(dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func (c *parityCLI) mustJSON(t *testing.T, v any, dir string, args ...string) {
	t.Helper()
	out := c.mustRun(t, dir, args...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("pulumi %s: not JSON: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// spec is a StackSpec on the same backend and passphrase as the CLI.
func (c *parityCLI) spec(name, dir string, create bool) StackSpec {
	proj := ProjectSpec{Dir: dir}
	if dir == "" {
		proj.Name = "engine-parity"
	}
	return StackSpec{
		Name:    name,
		Project: proj,
		Backend: BackendSpec{URL: c.backendURL()},
		Secrets: SecretsSpec{Provider: "passphrase", Passphrase: parityPassphrase},
		Create:  create,
	}
}

// exportedDeployment decodes an untyped deployment (from Stack.Export or
// `pulumi stack export`) into apitype.DeploymentV3.
func exportedDeployment(t *testing.T, raw []byte) (apitype.UntypedDeployment, apitype.DeploymentV3) {
	t.Helper()
	var untyped apitype.UntypedDeployment
	if err := json.Unmarshal(raw, &untyped); err != nil {
		t.Fatalf("export is not an untyped deployment: %v\n%s", err, raw)
	}
	if untyped.Version != apitype.DeploymentSchemaVersionCurrent {
		t.Fatalf("export version = %d, want %d", untyped.Version, apitype.DeploymentSchemaVersionCurrent)
	}
	var dep apitype.DeploymentV3
	if err := json.Unmarshal(untyped.Deployment, &dep); err != nil {
		t.Fatalf("deployment is not apitype.DeploymentV3: %v", err)
	}
	return untyped, dep
}

// stackOutputs returns the root stack resource's outputs from a deployment.
func stackOutputs(dep apitype.DeploymentV3) map[string]any {
	for _, r := range dep.Resources {
		if r.Type == resource.RootStackType && r.Parent == "" {
			return r.Outputs
		}
	}
	return nil
}

// isCiphertext reports whether v is a serialized secret ({sig, ciphertext}).
func isCiphertext(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if m[resource.SigKey] != resource.SecretSig {
		return false
	}
	ct, _ := m["ciphertext"].(string)
	return ct != ""
}

// resourceNames lists "type::name" for every resource in a deployment, sorted.
// The stack segment of the URN is deliberately dropped.
func resourceNames(dep apitype.DeploymentV3) []string {
	var out []string
	for _, r := range dep.Resources {
		name := r.URN.Name()
		if r.Type == resource.RootStackType {
			name = "<stack>"
		}
		out = append(out, string(r.Type)+"::"+name)
	}
	sort.Strings(out)
	return out
}

// eventSignature reduces an engine event to what should be identical between
// the CLI and the library: the kind, and for step events the op, type and
// name (not the URN's stack segment, not sequence numbers or timestamps).
func eventSignature(e apitype.EngineEvent) string {
	step := func(kind string, m apitype.StepEventMetadata) string {
		name := resource.URN(m.URN).Name()
		if m.Type == string(resource.RootStackType) {
			name = "<stack>" // the stack resource is named after the stack
		}
		return fmt.Sprintf("%s %s %s::%s", kind, m.Op, m.Type, name)
	}
	switch {
	case e.PreludeEvent != nil:
		keys := make([]string, 0, len(e.PreludeEvent.Config))
		for k := range e.PreludeEvent.Config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "prelude config=" + strings.Join(keys, ",")
	case e.SummaryEvent != nil:
		changes := make([]string, 0, len(e.SummaryEvent.ResourceChanges))
		for k, v := range e.SummaryEvent.ResourceChanges {
			changes = append(changes, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(changes)
		return fmt.Sprintf("summary preview=%v %s", e.SummaryEvent.IsPreview, strings.Join(changes, ","))
	case e.ResourcePreEvent != nil:
		return step("resourcePre", e.ResourcePreEvent.Metadata)
	case e.ResOutputsEvent != nil:
		return step("resOutputs", e.ResOutputsEvent.Metadata)
	case e.ResOpFailedEvent != nil:
		return step("resOpFailed", e.ResOpFailedEvent.Metadata)
	case e.DiagnosticEvent != nil:
		return "diagnostic " + e.DiagnosticEvent.Severity
	case e.CancelEvent != nil:
		return "cancel"
	case e.StdoutEvent != nil:
		// The library delivers the backend's "Updating (dev):" banner as a
		// stdout event; the CLI prints it to its stdout, outside the log.
		return "diagnostic stdout"
	case e.ProgressEvent != nil:
		return "progress"
	default:
		return "other"
	}
}

func readEventLog(t *testing.T, path string) []apitype.EngineEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("event log: %v", err)
	}
	defer f.Close()
	var events []apitype.EngineEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e apitype.EngineEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("event log line %q: %v", line, err)
		}
		events = append(events, e)
	}
	return events
}

func libraryAPIEvents(events []Event) []apitype.EngineEvent {
	out := make([]apitype.EngineEvent, 0, len(events))
	for _, e := range events {
		out = append(out, e.EngineEvent)
	}
	return out
}

// compareEventStreams checks that two event streams carry the same multiset
// of step events with the same envelope. Ordering among independent
// resources is not deterministic in either implementation, so the comparison
// is on sorted signatures. Diagnostics are compared by count per severity and
// reported, never as a hard failure, because their text depends on the
// environment (ambient plugin warnings and the like).
func compareEventStreams(t *testing.T, what string, cliEvents, libEvents []apitype.EngineEvent) {
	t.Helper()
	sigs := func(events []apitype.EngineEvent) (steps []string, diags map[string]int, first, last string) {
		diags = map[string]int{}
		for i, e := range events {
			s := eventSignature(e)
			if i == 0 {
				first = s
			}
			last = s
			switch {
			case strings.HasPrefix(s, "diagnostic"):
				diags[s]++
			case s == "cancel":
				// Both streams end with the engine's cancel terminator;
				// compared through first/last below, not as a step.
			default:
				steps = append(steps, s)
			}
		}
		sort.Strings(steps)
		return steps, diags, first, last
	}
	cliSteps, cliDiags, cliFirst, cliLast := sigs(cliEvents)
	libSteps, libDiags, libFirst, libLast := sigs(libEvents)
	if !reflect.DeepEqual(cliSteps, libSteps) {
		t.Errorf("%s: event streams differ\n  cli: %v\n  lib: %v", what, cliSteps, libSteps)
	} else {
		t.Logf("%s: %d step events agree: %v", what, len(cliSteps), cliSteps)
	}
	if !reflect.DeepEqual(cliDiags, libDiags) {
		t.Logf("%s: diagnostics differ (finding, not failure): cli=%v lib=%v", what, cliDiags, libDiags)
		for _, e := range cliEvents {
			if e.DiagnosticEvent != nil {
				t.Logf("  cli diagnostic %s: %s", e.DiagnosticEvent.Severity, strings.TrimSpace(e.DiagnosticEvent.Message))
			}
		}
		for _, e := range libEvents {
			if e.DiagnosticEvent != nil {
				t.Logf("  lib diagnostic %s: %s", e.DiagnosticEvent.Severity, strings.TrimSpace(e.DiagnosticEvent.Message))
			}
		}
	}
	firstNonDiag := func(events []apitype.EngineEvent) string {
		for _, e := range events {
			if e.DiagnosticEvent == nil && e.StdoutEvent == nil {
				return eventSignature(e)
			}
		}
		return ""
	}
	if a, b := firstNonDiag(cliEvents), firstNonDiag(libEvents); !strings.HasPrefix(a, "prelude") || a != b {
		t.Errorf("%s: first non-diagnostic event: cli %q lib %q", what, a, b)
	}
	if cliLast != "cancel" {
		t.Errorf("%s: CLI event log should end with the cancel terminator, got %q (first %q)", what, cliLast, cliFirst)
	}
	if libLast != "cancel" {
		t.Errorf("%s: library stream should end with the cancel terminator, got %q (first %q)", what, libLast, libFirst)
	}
	for i, e := range libEvents {
		if e.Sequence != i || e.Timestamp == 0 {
			t.Errorf("%s: library event %d has sequence %d timestamp %d", what, i, e.Sequence, e.Timestamp)
		}
	}
}

func waitOp(t *testing.T, op *Operation) (Result, []Event, error) {
	t.Helper()
	events := drain(t, op)
	res, err := op.Wait()
	return res, events, err
}

// TestIntegrationParityLibraryWritesCLIReads: the library deploys, the CLI
// reads the state, sees no changes, refreshes, destroys.
func TestIntegrationParityLibraryWritesCLIReads(t *testing.T) {
	c := newParityCLI(t)
	dir := copyFixture(t, "yaml-parity")
	ctx := context.Background()

	spec := c.spec("dev", dir, true)
	spec.Config = map[string]ConfigValue{
		"petLength": {Value: "3"},
		"apiKey":    {Value: "hunter2", Secret: true},
	}
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	res, events, err := waitOp(t, st.Up(ctx, localProgram(dir), Options{Message: "library up"}))
	if err != nil {
		t.Fatalf("library up: %v (%v)", err, eventTypes(events))
	}
	if res.Changes["create"] != 3 {
		t.Fatalf("library up changes = %v", res.Changes)
	}
	libOut, err := st.Outputs(ctx, true)
	if err != nil {
		t.Fatal(err)
	}

	// stack export: decodes as a v3 deployment, secrets are ciphertext, and
	// the CLI and the library export the same document.
	cliExport := c.mustRun(t, dir, "stack", "export", "--stack", "dev")
	_, dep := exportedDeployment(t, []byte(cliExport))
	if dep.SecretsProviders == nil || dep.SecretsProviders.Type != "passphrase" {
		t.Errorf("secrets provider in checkpoint = %+v, want passphrase", dep.SecretsProviders)
	}
	if got := resourceNames(dep); len(got) != 4 { // stack, provider, pet, token
		t.Errorf("checkpoint resources = %v", got)
	}
	outs := stackOutputs(dep)
	for _, k := range []string{"token", "apiKey"} {
		if !isCiphertext(outs[k]) {
			t.Errorf("output %s should be ciphertext in the checkpoint, got %v", k, outs[k])
		}
	}
	if pet, _ := outs["petName"].(string); pet != libOut.Values["petName"] {
		t.Errorf("petName in checkpoint = %v, library says %v", outs["petName"], libOut.Values["petName"])
	}
	if strings.Contains(cliExport, libOut.Values["token"].(string)) {
		t.Errorf("checkpoint contains the plaintext token")
	}
	libExport, err := st.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal([]byte(cliExport), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(libExport, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("CLI and library exports differ\ncli: %s\nlib: %s", cliExport, libExport)
	}

	// stack output: redacted without --show-secrets, plaintext with.
	var redacted, shown map[string]any
	c.mustJSON(t, &redacted, dir, "stack", "output", "--json", "--stack", "dev")
	c.mustJSON(t, &shown, dir, "stack", "output", "--json", "--show-secrets", "--stack", "dev")
	if redacted["token"] != "[secret]" || redacted["apiKey"] != "[secret]" {
		t.Errorf("CLI stack output without --show-secrets = %v", redacted)
	}
	if !reflect.DeepEqual(shown, libOut.Values) {
		t.Errorf("CLI --show-secrets outputs %v != library %v", shown, libOut.Values)
	}
	if shown["apiKey"] != "hunter2" {
		t.Errorf("secret config did not round-trip into outputs: %v", shown["apiKey"])
	}
	libRedacted, _ := st.Outputs(ctx, false)
	if !reflect.DeepEqual(redacted, libRedacted.Values) {
		t.Errorf("CLI redacted outputs %v != library %v", redacted, libRedacted.Values)
	}

	// config as the CLI sees it
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "petLength", "--stack", "dev")); got != "3" {
		t.Errorf("pulumi config get petLength = %q", got)
	}
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "apiKey", "--stack", "dev")); got != "hunter2" {
		t.Errorf("pulumi config get apiKey = %q", got)
	}

	// preview / refresh / destroy through the CLI
	c.mustRun(t, dir, "preview", "--expect-no-changes", "--stack", "dev")
	c.mustRun(t, dir, "refresh", "--yes", "--skip-preview", "--expect-no-changes", "--stack", "dev")
	c.mustRun(t, dir, "destroy", "--yes", "--skip-preview", "--stack", "dev")

	after, err := st.Outputs(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Values) != 0 {
		t.Errorf("library still sees outputs after CLI destroy: %v", after.Values)
	}
	_, dep = exportedDeployment(t, []byte(c.mustRun(t, dir, "stack", "export", "--stack", "dev")))
	if n := len(dep.Resources); n != 0 {
		t.Errorf("%d resources left after CLI destroy", n)
	}
	c.mustRun(t, dir, "stack", "rm", "--yes", "--stack", "dev")
	spec.Create = false
	if _, err := Open(ctx, spec); !errors.As(err, new(StackNotFound)) {
		t.Errorf("library open after CLI stack rm: %v, want StackNotFound", err)
	}
}

// TestIntegrationParityCLIWritesLibraryReads: the CLI deploys, the library
// reads the state, sees no changes, decrypts, refreshes, destroys; the CLI
// agrees on the outcome.
func TestIntegrationParityCLIWritesLibraryReads(t *testing.T) {
	c := newParityCLI(t)
	dir := copyFixture(t, "yaml-parity")
	ctx := context.Background()

	c.mustRun(t, dir, "stack", "init", "dev")
	c.mustRun(t, dir, "config", "set", "petLength", "3", "--stack", "dev")
	c.mustRun(t, dir, "config", "set", "--secret", "apiKey", "hunter2", "--stack", "dev")
	c.mustRun(t, dir, "up", "--yes", "--skip-preview", "--stack", "dev")

	var cliShown map[string]any
	c.mustJSON(t, &cliShown, dir, "stack", "output", "--json", "--show-secrets", "--stack", "dev")

	st, err := Open(ctx, c.spec("dev", dir, false))
	if err != nil {
		t.Fatalf("library open of CLI stack: %v", err)
	}
	// config written by the CLI
	if v, ok, err := st.GetConfig(ctx, "apiKey"); err != nil || !ok || v.Value != "hunter2" || !v.Secret {
		t.Errorf("GetConfig(apiKey) = %+v %v %v", v, ok, err)
	}
	if v, ok, err := st.GetConfig(ctx, "petLength"); err != nil || !ok || v.Value != "3" || v.Secret {
		t.Errorf("GetConfig(petLength) = %+v %v %v", v, ok, err)
	}

	res, events, err := waitOp(t, st.Preview(ctx, localProgram(dir), Options{}))
	if err != nil {
		t.Fatalf("library preview: %v (%v)", err, eventTypes(events))
	}
	if res.Changes["same"] != 3 || res.Changes["create"] != 0 || res.Changes["update"] != 0 {
		t.Errorf("library preview of CLI state should be all same, got %v", res.Changes)
	}

	shown, err := st.Outputs(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shown.Values, cliShown) {
		t.Errorf("library outputs %v != CLI --show-secrets %v", shown.Values, cliShown)
	}
	if tok, _ := shown.Values["token"].(string); len(tok) != 12 {
		t.Errorf("token = %q, want 12 chars", tok)
	}
	if !reflect.DeepEqual(shown.SecretKeys, []string{"apiKey", "token"}) {
		t.Errorf("secret keys = %v", shown.SecretKeys)
	}
	redacted, _ := st.Outputs(ctx, false)
	if redacted.Values["token"] != "[secret]" || redacted.Values["apiKey"] != "[secret]" {
		t.Errorf("redacted outputs = %v", redacted.Values)
	}

	res, events, err = waitOp(t, st.Refresh(ctx, nil, Options{}))
	if err != nil {
		t.Fatalf("library refresh: %v (%v)", err, eventTypes(events))
	}
	if res.Changes["same"] == 0 || res.Changes["update"] != 0 || res.Changes["delete"] != 0 {
		t.Errorf("library refresh of CLI state should be a no-op, got %v", res.Changes)
	}
	res, events, err = waitOp(t, st.Destroy(ctx, nil, Options{}))
	if err != nil {
		t.Fatalf("library destroy: %v (%v)", err, eventTypes(events))
	}
	if res.Changes["delete"] != 3 {
		t.Errorf("library destroy changes = %v", res.Changes)
	}

	var stacks []map[string]any
	c.mustJSON(t, &stacks, dir, "stack", "ls", "--json")
	found := false
	for _, s := range stacks {
		if s["name"] == "dev" {
			found = true
			if rc, _ := s["resourceCount"].(float64); rc != 0 {
				t.Errorf("CLI stack ls resourceCount = %v after library destroy", rc)
			}
		}
	}
	if !found {
		t.Errorf("CLI stack ls does not list dev: %v", stacks)
	}
	if err := st.Remove(ctx, false); err != nil {
		t.Fatalf("library remove: %v", err)
	}
	stacks = nil
	c.mustJSON(t, &stacks, dir, "stack", "ls", "--json")
	for _, s := range stacks {
		if s["name"] == "dev" {
			t.Errorf("CLI still lists dev after library remove: %v", stacks)
		}
	}
	if _, err := c.run(dir, "stack", "rm", "--yes", "--stack", "dev"); err == nil {
		t.Errorf("CLI stack rm should fail once the library removed the stack")
	}
}

// TestIntegrationParityEvents compares the event stream of `pulumi up
// --event-log` with the library's for the same program on fresh stacks, and
// likewise for destroy (whose library events arrive through the loopback
// sink rather than the engine channel).
func TestIntegrationParityEvents(t *testing.T) {
	c := newParityCLI(t)
	ctx := context.Background()
	cliDir := copyFixture(t, "yaml-parity")
	libDir := copyFixture(t, "yaml-parity")
	logDir := t.TempDir()

	c.mustRun(t, cliDir, "stack", "init", "cli")
	c.mustRun(t, cliDir, "config", "set", "petLength", "3", "--stack", "cli")
	c.mustRun(t, cliDir, "config", "set", "--secret", "apiKey", "hunter2", "--stack", "cli")
	upLog := filepath.Join(logDir, "cli-up.jsonl")
	c.mustRun(t, cliDir, "up", "--yes", "--skip-preview", "--event-log", upLog, "--stack", "cli")

	spec := c.spec("lib", libDir, true)
	spec.Config = map[string]ConfigValue{"petLength": {Value: "3"}, "apiKey": {Value: "hunter2", Secret: true}}
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, libUp, err := waitOp(t, st.Up(ctx, localProgram(libDir), Options{}))
	if err != nil {
		t.Fatalf("library up: %v", err)
	}
	compareEventStreams(t, "up", readEventLog(t, upLog), libraryAPIEvents(libUp))

	// Secrets are redacted the same way in both streams. (Both keep the
	// secret signature wrapper and blank the ciphertext:
	// {"4dabf...": "1b47...", "ciphertext": "[secret]"}.)
	redactedResult := func(what string, events []apitype.EngineEvent) string {
		for _, e := range events {
			if e.ResOutputsEvent != nil && strings.HasSuffix(string(e.ResOutputsEvent.Metadata.Type), "RandomPassword") {
				raw, _ := json.Marshal(e.ResOutputsEvent.Metadata.New.Outputs["result"])
				if !strings.Contains(string(raw), "[secret]") {
					t.Errorf("%s: RandomPassword result in events is not redacted: %s", what, raw)
				}
				return string(raw)
			}
		}
		t.Errorf("%s: no RandomPassword outputs event", what)
		return ""
	}
	if a, b := redactedResult("cli up", readEventLog(t, upLog)), redactedResult("lib up", libraryAPIEvents(libUp)); a != b {
		t.Errorf("redacted secret shapes differ: cli %s lib %s", a, b)
	} else {
		t.Logf("secret output in events (both): %s", a)
	}

	previewLog := filepath.Join(logDir, "cli-preview.jsonl")
	c.mustRun(t, cliDir, "preview", "--event-log", previewLog, "--stack", "cli")
	_, libPreview, err := waitOp(t, st.Preview(ctx, localProgram(libDir), Options{}))
	if err != nil {
		t.Fatalf("library preview: %v", err)
	}
	compareEventStreams(t, "preview", readEventLog(t, previewLog), libraryAPIEvents(libPreview))

	refreshLog := filepath.Join(logDir, "cli-refresh.jsonl")
	c.mustRun(t, cliDir, "refresh", "--yes", "--skip-preview", "--event-log", refreshLog, "--stack", "cli")
	_, libRefresh, err := waitOp(t, st.Refresh(ctx, nil, Options{}))
	if err != nil {
		t.Fatalf("library refresh: %v", err)
	}
	compareEventStreams(t, "refresh", readEventLog(t, refreshLog), libraryAPIEvents(libRefresh))

	destroyLog := filepath.Join(logDir, "cli-destroy.jsonl")
	c.mustRun(t, cliDir, "destroy", "--yes", "--skip-preview", "--event-log", destroyLog, "--stack", "cli")
	_, libDestroy, err := waitOp(t, st.Destroy(ctx, nil, Options{}))
	if err != nil {
		t.Fatalf("library destroy: %v", err)
	}
	compareEventStreams(t, "destroy", readEventLog(t, destroyLog), libraryAPIEvents(libDestroy))

	c.mustRun(t, cliDir, "stack", "rm", "--yes", "--stack", "cli")
	if err := st.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationParityConfig: config set by the library is read by the CLI
// and vice versa, from one Pulumi.<stack>.yaml.
func TestIntegrationParityConfig(t *testing.T) {
	c := newParityCLI(t)
	dir := copyFixture(t, "yaml-parity")
	ctx := context.Background()

	spec := c.spec("cfg", dir, true)
	// The project declares apiKey as required secret config; the CLI
	// validates the whole stack config on every `config` command.
	spec.Config = map[string]ConfigValue{"apiKey": {Value: "k", Secret: true}}
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfig(ctx, "libPlain", ConfigValue{Value: "from-library"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfig(ctx, "libSecret", ConfigValue{Value: "library-secret", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfig(ctx, "libObject", ConfigValue{Value: `{"a":[1,2]}`, Object: true}); err != nil {
		t.Fatal(err)
	}

	// library -> CLI
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "libPlain", "--stack", "cfg")); got != "from-library" {
		t.Errorf("config get libPlain = %q", got)
	}
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "libSecret", "--stack", "cfg")); got != "library-secret" {
		t.Errorf("config get libSecret = %q", got)
	}
	type cfgEntry struct {
		Value  string `json:"value"`
		Secret bool   `json:"secret"`
		Object any    `json:"objectValue"`
	}
	var redacted, shown map[string]cfgEntry
	c.mustJSON(t, &redacted, dir, "config", "--json", "--stack", "cfg")
	c.mustJSON(t, &shown, dir, "config", "--json", "--show-secrets", "--stack", "cfg")
	// Without --show-secrets the CLI omits the value entirely ({"secret": true}).
	if e := redacted["engine-parity:libSecret"]; !e.Secret || e.Value != "" {
		t.Errorf("config --json libSecret = %+v, want redacted secret", e)
	}
	if e := shown["engine-parity:libSecret"]; !e.Secret || e.Value != "library-secret" {
		t.Errorf("config --json --show-secrets libSecret = %+v", e)
	}
	if e := shown["engine-parity:libPlain"]; e.Secret || e.Value != "from-library" {
		t.Errorf("config --json libPlain = %+v", e)
	}
	var obj any
	if e := shown["engine-parity:libObject"]; json.Unmarshal([]byte(e.Value), &obj) != nil || !reflect.DeepEqual(obj, map[string]any{"a": []any{1.0, 2.0}}) {
		t.Errorf("config --json libObject = %+v", e)
	}

	// CLI -> library (a fresh handle re-reads Pulumi.cfg.yaml)
	c.mustRun(t, dir, "config", "set", "cliPlain", "from-cli", "--stack", "cfg")
	c.mustRun(t, dir, "config", "set", "--secret", "cliSecret", "cli-secret", "--stack", "cfg")
	c.mustRun(t, dir, "config", "set", "--path", "cliObject.a[0]", "1", "--stack", "cfg")
	st2, err := Open(ctx, c.spec("cfg", dir, false))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ConfigValue{
		"libPlain":  {Value: "from-library"},
		"libSecret": {Value: "library-secret", Secret: true},
		"cliPlain":  {Value: "from-cli"},
		"cliSecret": {Value: "cli-secret", Secret: true},
	}
	for k, w := range want {
		v, ok, err := st2.GetConfig(ctx, k)
		if err != nil || !ok || v != w {
			t.Errorf("GetConfig(%s) = %+v %v %v, want %+v", k, v, ok, err, w)
		}
	}
	if v, ok, _ := st2.GetConfig(ctx, "cliObject"); !ok || !v.Object || json.Unmarshal([]byte(v.Value), &obj) != nil ||
		!reflect.DeepEqual(obj, map[string]any{"a": []any{1.0}}) {
		t.Errorf("GetConfig(cliObject) = %+v %v", v, ok)
	}

	// The file both wrote is one CLI-shaped Pulumi.cfg.yaml: one salt,
	// project-scoped keys, secrets as {secure: ...}.
	path := filepath.Join(dir, "Pulumi.cfg.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sink := diag.DefaultSink(os.Stderr, os.Stderr, diag.FormatOptions{Color: colors.Never})
	proj, err := workspace.LoadProject(filepath.Join(dir, "Pulumi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := workspace.LoadProjectStack(sink, proj, path)
	if err != nil {
		t.Fatalf("Pulumi.cfg.yaml written by both is not loadable: %v\n%s", err, raw)
	}
	if ps.EncryptionSalt == "" || ps.SecretsProvider != "" {
		t.Errorf("Pulumi.cfg.yaml: encryptionsalt=%q secretsprovider=%q", ps.EncryptionSalt, ps.SecretsProvider)
	}
	for k, w := range want {
		v, ok, err := ps.Config.Get(mustKey(t, "engine-parity:"+k), false)
		if err != nil || !ok || v.Secure() != w.Secret {
			t.Errorf("Pulumi.cfg.yaml %s: %+v %v %v (want secure=%v)", k, v, ok, err, w.Secret)
		}
	}
	if strings.Contains(string(raw), "library-secret") || strings.Contains(string(raw), "cli-secret") {
		t.Errorf("Pulumi.cfg.yaml contains plaintext secrets:\n%s", raw)
	}
	// The CLI must also accept the library's view of the file after the
	// library rewrites it.
	if err := st2.SetConfig(ctx, "cliPlain", ConfigValue{Value: "rewritten"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "cliSecret", "--stack", "cfg")); got != "cli-secret" {
		t.Errorf("after library rewrite, config get cliSecret = %q", got)
	}
	if got := strings.TrimSpace(c.mustRun(t, dir, "config", "get", "cliPlain", "--stack", "cfg")); got != "rewritten" {
		t.Errorf("after library rewrite, config get cliPlain = %q", got)
	}
	if err := st2.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// waitForEventLog polls a CLI event log until pred matches an event.
func waitForEventLog(t *testing.T, path string, timeout time.Duration, pred func(apitype.EngineEvent) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				var e apitype.EngineEvent
				if json.Unmarshal([]byte(line), &e) == nil && pred(e) {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("event log %s: condition not met within %v", path, timeout)
}

func isResourcePreOf(name string) func(apitype.EngineEvent) bool {
	return func(e apitype.EngineEvent) bool {
		return e.ResourcePreEvent != nil && strings.HasSuffix(e.ResourcePreEvent.Metadata.URN, "::"+name)
	}
}

// TestIntegrationParityLockAndCancel: each side's in-flight up locks the
// other out; a library cancel and a CLI SIGINT leave the same checkpoint,
// which the other side can refresh and destroy.
func TestIntegrationParityLockAndCancel(t *testing.T) {
	c := newParityCLI(t)
	ctx := context.Background()
	logDir := t.TempDir()

	// --- library holds the lock; CLI up refuses; library cancel.
	libDir := copyFixture(t, "yaml-slow")
	st, err := Open(ctx, c.spec("lib", libDir, true))
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	op := st.Up(cctx, localProgram(libDir), Options{})
	inFlight := make(chan struct{})
	go func() {
		signalled := false
		for e := range op.Events() {
			if !signalled && e.ResourcePreEvent != nil && strings.HasSuffix(e.ResourcePreEvent.Metadata.URN, "::slow1") {
				signalled = true
				close(inFlight)
			}
		}
		if !signalled {
			close(inFlight)
		}
	}()
	select {
	case <-inFlight:
	case <-time.After(2 * time.Minute):
		t.Fatal("library up never reached slow1")
	}
	out, err := c.run(libDir, "up", "--yes", "--skip-preview", "--stack", "lib")
	if err == nil {
		t.Fatalf("CLI up should fail while the library holds the lock; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "conflict") {
		t.Errorf("CLI lock error does not mention the lock: %v", err)
	}
	t.Logf("CLI while library up in flight: %s", firstLine(err.Error(), "locked"))

	cancel()
	res, err := op.Wait()
	if !errors.As(err, new(Cancelled)) || !res.Cancelled {
		t.Fatalf("library cancel: %v", err)
	}
	libCancelled := c.mustRun(t, libDir, "stack", "export", "--stack", "lib")
	_, libDep := exportedDeployment(t, []byte(libCancelled))
	t.Logf("after library cancel: resources=%v pending=%d", resourceNames(libDep), len(libDep.PendingOperations))

	// --- CLI holds the lock; library up reports ConcurrentUpdate; SIGINT.
	cliDir := copyFixture(t, "yaml-slow")
	c.mustRun(t, cliDir, "stack", "init", "cli")
	cliLog := filepath.Join(logDir, "cli-up.jsonl")
	cmd := c.cmd(cliDir, "up", "--yes", "--skip-preview", "--event-log", cliLog, "--stack", "cli")
	var cliOut strings.Builder
	cmd.Stdout, cmd.Stderr = &cliOut, &cliOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForEventLog(t, cliLog, 2*time.Minute, isResourcePreOf("slow1"))

	st2, err := Open(ctx, c.spec("cli", cliDir, false))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, _, err = waitOp(t, st2.Up(ctx, LocalProgram{Dir: cliDir}, Options{}))
	var conc ConcurrentUpdate
	if !errors.As(err, &conc) {
		t.Errorf("library up while CLI holds the lock: %T %v, want ConcurrentUpdate", err, err)
	} else {
		t.Logf("library while CLI up in flight (%v): %v", time.Since(started).Round(time.Millisecond), firstLine(err.Error(), "locked"))
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	t.Logf("CLI after SIGINT: exit=%v\n%s", waitErr, strings.TrimSpace(cliOut.String()))
	cliExport, err := st2.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, cliDep := exportedDeployment(t, cliExport)
	t.Logf("after CLI SIGINT: resources=%v pending=%d", resourceNames(cliDep), len(cliDep.PendingOperations))

	// Both cancellations must leave the same shape of checkpoint.
	if a, b := resourceNames(libDep), resourceNames(cliDep); !reflect.DeepEqual(a, b) {
		t.Errorf("checkpoints after cancel differ: library-cancelled %v, CLI-interrupted %v", a, b)
	}
	if a, b := len(libDep.PendingOperations), len(cliDep.PendingOperations); a != b {
		t.Errorf("pending operations after cancel: library %d, CLI %d", a, b)
	}
	if len(libDep.PendingOperations) != 0 {
		t.Logf("finding: graceful cancel leaves pending operations: %v", libDep.PendingOperations)
	}

	// Each side recovers the other's cancelled stack.
	c.mustRun(t, libDir, "refresh", "--yes", "--skip-preview", "--stack", "lib")
	c.mustRun(t, libDir, "destroy", "--yes", "--skip-preview", "--stack", "lib")
	c.mustRun(t, libDir, "stack", "rm", "--yes", "--stack", "lib")
	if _, _, err := waitOp(t, st2.Refresh(ctx, nil, Options{})); err != nil {
		t.Fatalf("library refresh of CLI-interrupted stack: %v", err)
	}
	if _, _, err := waitOp(t, st2.Destroy(ctx, nil, Options{})); err != nil {
		t.Fatalf("library destroy of CLI-interrupted stack: %v", err)
	}
	if err := st2.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func firstLine(s, containing string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, containing) {
			return strings.TrimSpace(l)
		}
	}
	return strings.SplitN(s, "\n", 2)[0]
}

func mustKey(t *testing.T, s string) config.Key {
	t.Helper()
	k, err := config.ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestIntegrationParityPlugins: the library and the CLI share one plugin
// cache under PULUMI_HOME, and both honour an ambient pulumi-resource-*
// launcher on PATH in the same way.
func TestIntegrationParityPlugins(t *testing.T) {
	c := newParityCLI(t)
	ctx := context.Background()

	// A private PULUMI_HOME for both processes.
	home := t.TempDir()
	t.Setenv("PULUMI_HOME", home)
	// Language hosts ship next to the CLI binary; keep them reachable for
	// the CLI without depending on what else is on PATH.
	origPath := os.Getenv("PATH")
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+filepath.Dir(c.bin)+string(os.PathListSeparator)+origPath)

	t.Run("shared cache", func(t *testing.T) {
		t.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")
		const version = "4.16.8" // what the Go SDK pinned in go.mod requires
		c.mustRun(t, home, "plugin", "install", "resource", "random", version)

		pluginDir, err := workspace.GetPluginDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(pluginDir, home) {
			t.Fatalf("library plugin dir %s is not under PULUMI_HOME %s", pluginDir, home)
		}
		sink := diag.DefaultSink(os.Stderr, os.Stderr, diag.FormatOptions{Color: colors.Never})
		path, err := workspace.GetPluginPath(ctx, sink, workspace.PluginDescriptor{
			Kind: apitype.ResourcePlugin, Name: "random",
		}, nil)
		if err != nil {
			t.Fatalf("library cannot resolve the plugin the CLI installed: %v", err)
		}
		if !strings.HasPrefix(path, pluginDir) {
			t.Errorf("library resolved random to %s, not under the shared cache %s", path, pluginDir)
		}
		t.Logf("library resolves random -> %s", path)

		var cliPlugins []map[string]any
		c.mustJSON(t, &cliPlugins, home, "plugin", "ls", "--json")
		libPlugins, err := workspace.GetPlugins()
		if err != nil {
			t.Fatal(err)
		}
		list := func(name, ver string) string { return name + "@" + ver }
		var cliList, libList []string
		for _, p := range cliPlugins {
			cliList = append(cliList, list(p["name"].(string), fmt.Sprint(p["version"])))
		}
		for _, p := range libPlugins {
			libList = append(libList, list(p.Name, p.Version.String()))
		}
		sort.Strings(cliList)
		sort.Strings(libList)
		if !reflect.DeepEqual(cliList, libList) {
			t.Errorf("plugin lists differ: cli %v, library %v", cliList, libList)
		}

		before, _ := os.ReadDir(pluginDir)
		st, err := Open(ctx, c.spec("cache", "", true))
		if err != nil {
			t.Fatal(err)
		}
		res, events, err := waitOp(t, st.Up(ctx, GoProgram(func(ctx *pulumi.Context) error {
			_, err := random.NewRandomPet(ctx, "pet", nil)
			return err
		}), Options{}))
		if err != nil {
			t.Fatalf("library up with the CLI-installed plugin: %v (%v)", err, diagnostics(events))
		}
		if res.Changes["create"] != 2 {
			t.Errorf("changes = %v", res.Changes)
		}
		after, _ := os.ReadDir(pluginDir)
		if len(before) != len(after) {
			t.Errorf("library changed the plugin cache (%d -> %d entries); it should have used the CLI's install", len(before), len(after))
		}
		if _, _, err := waitOp(t, st.Destroy(ctx, nil, Options{})); err != nil {
			t.Fatal(err)
		}
		if err := st.Remove(ctx, false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ambient launcher", func(t *testing.T) {
		// A pulumi-resource-random on PATH that records who launched it and
		// then execs the cached plugin.
		sink := diag.DefaultSink(os.Stderr, os.Stderr, diag.FormatOptions{Color: colors.Never})
		os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")
		real, err := workspace.GetPluginPath(ctx, sink, workspace.PluginDescriptor{
			Kind: apitype.ResourcePlugin, Name: "random",
		}, nil)
		os.Unsetenv("PULUMI_IGNORE_AMBIENT_PLUGINS")
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "launched")
		script := fmt.Sprintf("#!/bin/sh\necho \"ppid=$PPID\" >> %q\nexec %q \"$@\"\n", marker, real)
		launcher := filepath.Join(binDir, "pulumi-resource-random")
		if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(launcher)

		resolved, err := workspace.GetPluginPath(ctx, sink, workspace.PluginDescriptor{
			Kind: apitype.ResourcePlugin, Name: "random",
		}, nil)
		if err != nil || resolved != launcher {
			t.Fatalf("library resolves random -> %q (%v), want the ambient launcher %s", resolved, err, launcher)
		}

		launchedBy := func() map[string]int {
			counts := map[string]int{}
			data, _ := os.ReadFile(marker)
			for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if l != "" {
					counts[l]++
				}
			}
			return counts
		}

		st, err := Open(ctx, c.spec("ambient", "", true))
		if err != nil {
			t.Fatal(err)
		}
		_, events, err := waitOp(t, st.Preview(ctx, GoProgram(func(ctx *pulumi.Context) error {
			_, err := random.NewRandomPet(ctx, "pet", nil)
			return err
		}), Options{}))
		if err != nil {
			t.Fatalf("library preview through the launcher: %v", err)
		}
		me := fmt.Sprintf("ppid=%d", os.Getpid())
		byLib := launchedBy()
		if byLib[me] == 0 {
			t.Errorf("launcher was not started by the library process (%s): %v", me, byLib)
		}
		ambientWarned := false
		for _, d := range diagnostics(events) {
			if strings.Contains(d, "$PATH") || strings.Contains(d, "PATH") {
				ambientWarned = true
			}
		}
		t.Logf("library launched the ambient plugin %d time(s); warned about it: %v", byLib[me], ambientWarned)
		if err := st.Remove(ctx, true); err != nil {
			t.Fatal(err)
		}

		dir := copyFixture(t, "yaml-parity")
		c.mustRun(t, dir, "stack", "init", "ambient-cli")
		c.mustRun(t, dir, "config", "set", "--secret", "apiKey", "x", "--stack", "ambient-cli")
		cmd := c.cmd(dir, "preview", "--stack", "ambient-cli")
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("CLI preview through the launcher: %v\n%s", err, out.String())
		}
		byCLI := launchedBy()
		cliKey := fmt.Sprintf("ppid=%d", cmd.ProcessState.Pid())
		if byCLI[cliKey] == 0 {
			t.Errorf("launcher was not started by the CLI process (%s): %v", cliKey, byCLI)
		}
		t.Logf("CLI launched the ambient plugin %d time(s); warned: %v", byCLI[cliKey], strings.Contains(out.String(), "PATH"))
		c.mustRun(t, dir, "stack", "rm", "--yes", "--stack", "ambient-cli")
	})
}
