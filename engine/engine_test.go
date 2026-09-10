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

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pulumi/pulumi/pkg/v3/backend/backenderr"
	pulumiengine "github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestSpecValidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Pulumi.yaml"), []byte("name: fromfile\nruntime: yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := func() StackSpec {
		return StackSpec{
			Name:    "dev",
			Project: ProjectSpec{Name: "proj"},
			Backend: BackendSpec{URL: "file:///tmp/state"},
			Secrets: SecretsSpec{Provider: "passphrase", Passphrase: "pw"},
		}
	}
	cases := []struct {
		name   string
		mutate func(*StackSpec)
		field  string // expected InvalidSpec field, "" for valid
		check  func(t *testing.T, s StackSpec)
	}{
		{name: "valid", mutate: func(*StackSpec) {}},
		{name: "missing name", mutate: func(s *StackSpec) { s.Name = "" }, field: "name"},
		{name: "bad name", mutate: func(s *StackSpec) { s.Name = "has space" }, field: "name"},
		{name: "qualified name ok", mutate: func(s *StackSpec) { s.Name = "org/proj/dev" }},
		{name: "missing backend", mutate: func(s *StackSpec) { s.Backend.URL = "" }, field: "backend.url"},
		{name: "odd backend", mutate: func(s *StackSpec) { s.Backend.URL = "ftp://x" }, field: "backend.url"},
		{name: "token on diy", mutate: func(s *StackSpec) { s.Backend.Token = "t" }, field: "backend.token"},
		{name: "missing project", mutate: func(s *StackSpec) { s.Project.Name = "" }, field: "project.name"},
		{name: "project from dir", mutate: func(s *StackSpec) { s.Project.Name = ""; s.Project.Dir = dir },
			check: func(t *testing.T, s StackSpec) {
				if s.Project.Name != "fromfile" {
					t.Errorf("project name not read from Pulumi.yaml: %q", s.Project.Name)
				}
			}},
		{name: "bad dir", mutate: func(s *StackSpec) { s.Project.Dir = filepath.Join(dir, "nope") }, field: "project.dir"},
		{name: "passphrase default for diy", mutate: func(s *StackSpec) { s.Secrets = SecretsSpec{} }, field: "secrets.passphrase"},
		{name: "service default for http", mutate: func(s *StackSpec) {
			s.Backend.URL = "https://api.pulumi.com"
			s.Secrets = SecretsSpec{}
		}, check: func(t *testing.T, s StackSpec) {
			if s.Secrets.Provider != "service" {
				t.Errorf("default provider for http backend = %q", s.Secrets.Provider)
			}
		}},
		{name: "service on diy", mutate: func(s *StackSpec) { s.Secrets = SecretsSpec{Provider: "service"} }, field: "secrets.provider"},
		{name: "unknown provider", mutate: func(s *StackSpec) { s.Secrets = SecretsSpec{Provider: "vault"} }, field: "secrets.provider"},
		{name: "kms ok", mutate: func(s *StackSpec) { s.Secrets = SecretsSpec{Provider: "awskms://alias/x?region=us-east-1"} }},
		{name: "passphrase with kms", mutate: func(s *StackSpec) {
			s.Secrets = SecretsSpec{Provider: "gcpkms://x", Passphrase: "pw"}
		}, field: "secrets.passphrase"},
		{name: "b64 ok", mutate: func(s *StackSpec) { s.Secrets = SecretsSpec{Provider: "b64"} }},
		{name: "object config not json", mutate: func(s *StackSpec) {
			s.Config = map[string]ConfigValue{"k": {Value: "not json", Object: true}}
		}, field: "config.k"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := good()
			c.mutate(&s)
			err := s.validate()
			if c.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if c.check != nil {
					c.check(t, s)
				}
				return
			}
			var inv InvalidSpec
			if !errors.As(err, &inv) {
				t.Fatalf("expected InvalidSpec, got %v", err)
			}
			if inv.Field != c.field {
				t.Errorf("field = %q, want %q (%v)", inv.Field, c.field, err)
			}
			if KindOf(err) != KindInvalidSpec {
				t.Errorf("KindOf = %s", KindOf(err))
			}
		})
	}
}

func TestErrorKinds(t *testing.T) {
	cases := map[ErrorKind]error{
		KindInvalidSpec:       InvalidSpec{Field: "x", Message: "m"},
		KindResourceOpFailed:  ResourceOpFailed{URN: "urn:pulumi:a::b::c::d", Op: "create"},
		KindProgramFailed:     ProgramFailed{Message: "boom"},
		KindConcurrentUpdate:  ConcurrentUpdate{Err: errors.New("locked")},
		KindStackNotFound:     StackNotFound{Name: "dev"},
		KindStackExists:       StackExists{Name: "dev"},
		KindPendingOperations: PendingOperations{URNs: []string{"u"}, Err: errors.New("pending")},
		KindCancelled:         Cancelled{Operation: KindUp},
		KindUnclassified:      Unclassified{Err: errors.New("?")},
	}
	for kind, err := range cases {
		if got := KindOf(err); got != kind {
			t.Errorf("KindOf(%T) = %s, want %s", err, got, kind)
		}
		if err.Error() == "" {
			t.Errorf("%T has empty message", err)
		}
	}
	if KindOf(errors.New("plain")) != KindUnclassified {
		t.Errorf("plain errors are unclassified")
	}
}

func TestClassifyBackendError(t *testing.T) {
	if err := classifyBackendError("dev", nil); err != nil {
		t.Errorf("nil stays nil")
	}
	var nf StackNotFound
	if err := classifyBackendError("dev", backenderr.StackNotFoundError{StackName: "dev"}); !errors.As(err, &nf) || nf.Name != "dev" {
		t.Errorf("not found: %v", err)
	}
	var ex StackExists
	if err := classifyBackendError("dev", backenderr.StackAlreadyExistsError{StackName: "dev"}); !errors.As(err, &ex) {
		t.Errorf("exists: %v", err)
	}
	var cu ConcurrentUpdate
	if err := classifyBackendError("dev", backenderr.ConflictingUpdateError{Err: errors.New("x")}); !errors.As(err, &cu) {
		t.Errorf("conflict: %v", err)
	}
	if err := classifyBackendError("dev", errors.New("the stack is currently locked by 1 lock(s)")); !errors.As(err, &cu) {
		t.Errorf("diy lock: %v", err)
	}
	var un Unclassified
	if err := classifyBackendError("dev", errors.New("weird")); !errors.As(err, &un) {
		t.Errorf("unclassified: %v", err)
	}
}

func TestClassifyOperationError(t *testing.T) {
	newOp := func() *Operation {
		return newOperation(KindUp, &Stack{running: map[*Operation]struct{}{}})
	}
	t.Run("resource failure wins and gets its diagnostics", func(t *testing.T) {
		op := newOp()
		op.handleEvent(eventFromAPI(diagnosticEventFor("urn:pulumi:a::b::t::r", "provider said no")))
		op.handleEvent(eventFromAPI(resOpFailedEvent("urn:pulumi:a::b::t::r", "t", "create")))
		op.handleEvent(eventFromAPI(diagnosticEventFor("", "update failed")))
		op.fillFailureMessages()
		err := op.classifyOperationError(errors.New("update failed"))
		var rof ResourceOpFailed
		if !errors.As(err, &rof) {
			t.Fatalf("got %T: %v", err, err)
		}
		if rof.Message != "provider said no" || rof.Op != "create" || rof.Type != "t" {
			t.Errorf("failure = %+v", rof)
		}
	})
	t.Run("program failure from diagnostics", func(t *testing.T) {
		op := newOp()
		op.handleEvent(eventFromAPI(diagnosticEventFor("urn:pulumi:a::b::pulumi:pulumi:Stack::b-a", "kaboom")))
		op.fillFailureMessages()
		err := op.classifyOperationError(errors.New("update failed"))
		var pf ProgramFailed
		if !errors.As(err, &pf) || !strings.Contains(pf.Message, "kaboom") {
			t.Fatalf("got %T: %v", err, err)
		}
	})
	t.Run("cancel requested", func(t *testing.T) {
		op := newOp()
		op.Cancel()
		var c Cancelled
		if err := op.classifyOperationError(errors.New("canceled")); !errors.As(err, &c) || c.Operation != KindUp {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		op := newOp()
		if _, ok := op.classifyOperationError(context.Canceled).(Cancelled); !ok {
			t.Fatalf("context.Canceled should map to Cancelled")
		}
	})
	t.Run("pending operations", func(t *testing.T) {
		op := newOp()
		op.pendingFn = func() []string { return []string{"urn:x"} }
		var p PendingOperations
		err := op.classifyOperationError(errors.New("the current deployment has 1 resource(s) with pending operations"))
		if !errors.As(err, &p) || !reflect.DeepEqual(p.URNs, []string{"urn:x"}) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		op := newOp()
		var cu ConcurrentUpdate
		if err := op.classifyOperationError(backenderr.ConflictingUpdateError{Err: errors.New("x")}); !errors.As(err, &cu) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unclassified", func(t *testing.T) {
		op := newOp()
		var un Unclassified
		if err := op.classifyOperationError(errors.New("?")); !errors.As(err, &un) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("nil", func(t *testing.T) {
		if err := newOp().classifyOperationError(nil); err != nil {
			t.Fatalf("got %v", err)
		}
	})
}

func TestHandleEventStripsColour(t *testing.T) {
	op := newOperation(KindUp, &Stack{running: map[*Operation]struct{}{}})
	ev := eventFromAPI(diagnosticEventFor("", "<{%reset%}>red<{%reset%}> text"))
	op.handleEvent(ev)
	got := <-op.Events()
	if got.DiagnosticEvent.Message != "red text\n" {
		t.Errorf("colour markup not stripped: %q", got.DiagnosticEvent.Message)
	}
}

// TestEventFixtures replays events recorded from real runs (see
// integration_test.go, PULUMI_ENGINE_RECORD_DIR) through the JSON decoder.
func TestEventFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "events", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var events []Event
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := sc.Bytes()
				var typed struct {
					Type EventType `json:"type"`
				}
				if err := json.Unmarshal(line, &typed); err != nil {
					t.Fatal(err)
				}
				ev, err := ParseEventJSON(line)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if ev.Type != typed.Type {
					t.Errorf("type recomputed as %s, recorded %s", ev.Type, typed.Type)
				}
				if ev.Type == EventUnknown {
					t.Errorf("fixture contains %s event", ev.Type)
				}
				// round trip is lossless
				again, err := json.Marshal(ev)
				if err != nil {
					t.Fatal(err)
				}
				var a, b any
				_ = json.Unmarshal(line, &a)
				_ = json.Unmarshal(again, &b)
				if !reflect.DeepEqual(a, b) {
					t.Errorf("round trip changed the event:\n%s\n%s", line, again)
				}
				events = append(events, ev)
			}
			if len(events) == 0 {
				t.Fatalf("empty fixture")
			}
			if n := len(events); events[n-1].Type != EventCancel || events[n-2].Type != EventSummary {
				t.Errorf("stream should end with summary then cancel, got %v", eventTypes(events[max(0, n-3):]))
			}
			for i, e := range events {
				if e.Sequence != i || e.Timestamp == 0 {
					t.Errorf("event %d: sequence %d timestamp %d", i, e.Sequence, e.Timestamp)
				}
			}
			for _, e := range events {
				raw, _ := json.Marshal(e)
				s := string(raw)
				if strings.Contains(s, "RandomPassword") && strings.Contains(s, "\"result\"") && !strings.Contains(s, "[secret]") {
					t.Errorf("password result is not redacted: %s", s)
				}
				if strings.Contains(s, "<{%") {
					t.Errorf("colour markup leaked into the event: %s", s)
				}
			}
			if strings.Contains(filepath.Base(file), "ResourceFailure") && strings.HasSuffix(file, "-up.jsonl") {
				found := false
				for _, e := range events {
					if e.Type == EventResourceOpFailed {
						found = true
					}
				}
				if !found {
					t.Errorf("failure fixture has no resourceOpFailed event")
				}
			}
		})
	}
}

func TestConvertEngineEventRedactsSecrets(t *testing.T) {
	urn := resource.NewURN("dev", "proj", "", "random:index/randomPassword:RandomPassword", "pw")
	state := &resource.State{
		Type: urn.Type(), URN: urn, ID: "id",
		Outputs: resource.PropertyMap{"result": resource.MakeSecret(resource.NewStringProperty("hunter2"))},
	}
	meta := pulumiengine.StepEventMetadata{
		Op: deploy.OpCreate, URN: urn, Type: urn.Type(),
		New: &pulumiengine.StepEventStateMetadata{Type: urn.Type(), URN: urn, ID: "id", State: state, Outputs: state.Outputs},
	}
	e := pulumiengine.NewEvent(pulumiengine.ResourceOutputsEventPayload{Metadata: meta})

	redacted, err := convertEngineEvent(e, false)
	if err != nil {
		t.Fatal(err)
	}
	if redacted.Type != EventResourceOutputs {
		t.Errorf("type = %s", redacted.Type)
	}
	raw, _ := json.Marshal(redacted)
	if strings.Contains(string(raw), "hunter2") || !strings.Contains(string(raw), "[secret]") {
		t.Errorf("secret leaked: %s", raw)
	}
	shown, err := convertEngineEvent(e, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(shown)
	if !strings.Contains(string(raw), "hunter2") {
		t.Errorf("showSecrets did not reveal the value: %s", raw)
	}
}

func TestEventQueue(t *testing.T) {
	q := newEventQueue()
	out := make(chan Event)
	go q.pump(out)
	for i := 0; i < 100; i++ {
		q.push(eventFromAPI(diagnosticEventFor("", string(rune('a'+i%26)))))
	}
	q.close()
	q.push(eventFromAPI(diagnosticEventFor("", "late"))) // ignored
	n := 0
	for e := range out {
		if want := string(rune('a' + n%26)); e.DiagnosticEvent.Message != want+"\n" {
			t.Errorf("event %d = %q, want %q", n, e.DiagnosticEvent.Message, want)
		}
		n++
	}
	if n != 100 {
		t.Errorf("delivered %d events, want 100", n)
	}
}

// TestScopeCancellation drives the backend-facing cancellation scope with a
// fake engine: no backend, no program, just the cancel context Pulumi's
// engine observes.
func TestScopeCancellation(t *testing.T) {
	op := newOperation(KindUp, &Stack{running: map[*Operation]struct{}{}})
	sc := scopeSource{op: op}.NewScope(context.Background(), nil, false)
	defer sc.Close()
	cctx := sc.Context()
	select {
	case <-cctx.Canceled():
		t.Fatalf("cancelled before anyone asked")
	default:
	}
	op.Cancel()
	select {
	case <-cctx.Canceled():
	case <-time.After(2 * time.Second):
		t.Fatalf("first Cancel did not cancel the engine context")
	}
	select {
	case <-cctx.Terminated():
		t.Fatalf("first Cancel must not terminate")
	default:
	}
	op.Cancel()
	select {
	case <-cctx.Terminated():
	case <-time.After(2 * time.Second):
		t.Fatalf("second Cancel did not terminate the engine context")
	}
	if !op.cancelRequested.Load() {
		t.Errorf("cancelRequested not recorded")
	}
}

func TestScopeContextCancellation(t *testing.T) {
	op := newOperation(KindUp, &Stack{running: map[*Operation]struct{}{}})
	ctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, op.Cancel)
	defer stop()
	sc := scopeSource{op: op}.NewScope(context.Background(), nil, false)
	defer sc.Close()
	cancel()
	select {
	case <-sc.Context().Canceled():
	case <-time.After(2 * time.Second):
		t.Fatalf("context cancellation did not reach the engine")
	}
}

func offlineSpec(t *testing.T, name string) StackSpec {
	t.Helper()
	return StackSpec{
		Name:    name,
		Project: ProjectSpec{Name: "offline", Dir: t.TempDir()},
		Backend: BackendSpec{URL: "file://" + t.TempDir()},
		Secrets: SecretsSpec{Provider: "b64"},
		Create:  true,
	}
}

// TestOfflineLifecycle runs the whole stack lifecycle against a file backend
// with a program that only exports values, so no provider plugin, language
// host or network is involved.
func TestOfflineLifecycle(t *testing.T) {
	ctx := context.Background()
	spec := offlineSpec(t, "dev")
	spec.Config = map[string]ConfigValue{"greeting": {Value: "hi"}, "token": {Value: "s3cret", Secret: true}}
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spec.Project.Dir, "Pulumi.dev.yaml")); err != nil {
		t.Errorf("stack config file not written: %v", err)
	}
	program := GoProgram(func(ctx *pulumi.Context) error {
		g, _ := ctx.GetConfig("offline:greeting")
		tok, _ := ctx.GetConfig("offline:token")
		ctx.Export("greeting", pulumi.String(g))
		ctx.Export("tokenLen", pulumi.Int(len(tok)))
		ctx.Export("secretOut", pulumi.ToSecret(pulumi.String("shh")))
		return nil
	})

	op := st.Preview(ctx, program, Options{})
	var types []EventType
	for e := range op.Events() {
		types = append(types, e.Type)
	}
	res, err := op.Wait()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Changes["create"] != 1 || !res.Summary.IsPreview {
		t.Errorf("preview result %+v", res)
	}
	// The DIY backend's "Previewing update (dev):" banner is captured from
	// the process stdout into a stdout event; the stream ends with the
	// engine's cancel terminator after the summary.
	if types[0] != EventStdout || types[1] != EventPrelude || types[len(types)-2] != EventSummary ||
		types[len(types)-1] != EventCancel {
		t.Errorf("preview events %v", types)
	}

	op = st.Up(ctx, program, Options{})
	for range op.Events() {
	}
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if res.Outputs == nil || res.Outputs.Values["greeting"] != "hi" || res.Outputs.Values["tokenLen"] != float64(6) {
		t.Errorf("outputs %+v", res.Outputs)
	}
	if res.Outputs.Values["secretOut"] != "[secret]" || !reflect.DeepEqual(res.Outputs.SecretKeys, []string{"secretOut"}) {
		t.Errorf("secret output not redacted/listed: %+v", res.Outputs)
	}
	out, err := st.Outputs(ctx, true)
	if err != nil || out.Values["secretOut"] != "shh" {
		t.Errorf("Outputs(showSecrets): %+v %v", out, err)
	}

	v, ok, err := st.GetConfig(ctx, "token")
	if err != nil || !ok || v.Value != "s3cret" || !v.Secret {
		t.Errorf("GetConfig(token) = %+v %v %v", v, ok, err)
	}
	if err := st.SetConfig(ctx, "greeting", ConfigValue{Value: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetConfig(ctx, "missing"); ok {
		t.Errorf("missing key reported present")
	}

	dep, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(string(dep), "pulumi:pulumi:Stack") {
		t.Errorf("export lacks the stack resource: %s", dep)
	}
	if err := st.Import(ctx, dep); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := st.Import(ctx, json.RawMessage(`{"nope": 1}`)); KindOf(err) != KindInvalidSpec {
		t.Errorf("bad import: %v", err)
	}

	// same-process concurrent update: second one must fail with ConcurrentUpdate
	op = st.Refresh(ctx, nil, Options{})
	for range op.Events() {
	}
	if _, err := op.Wait(); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	op = st.Destroy(ctx, nil, Options{})
	for range op.Events() {
	}
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if res.Changes["delete"] != 1 {
		t.Errorf("destroy changes %v", res.Changes)
	}
	if err := st.Remove(ctx, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	spec.Create = false
	if _, err := Open(ctx, spec); KindOf(err) != KindStackNotFound {
		t.Errorf("reopen: %v", err)
	}
}

func TestOfflineProgramRequired(t *testing.T) {
	st, err := Open(context.Background(), offlineSpec(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	op := st.Up(context.Background(), nil, Options{})
	if _, err := op.Wait(); KindOf(err) != KindInvalidSpec {
		t.Errorf("up without program: %v", err)
	}
	op = st.Up(context.Background(), CallbackProgram{}, Options{})
	if _, err := op.Wait(); KindOf(err) != KindInvalidSpec {
		t.Errorf("callback without address: %v", err)
	}
	op = st.Up(context.Background(), LocalProgram{Dir: t.TempDir()}, Options{})
	if _, err := op.Wait(); KindOf(err) != KindInvalidSpec {
		t.Errorf("local without Pulumi.yaml: %v", err)
	}
}

func TestOfflineProgramError(t *testing.T) {
	st, err := Open(context.Background(), offlineSpec(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	op := st.Up(context.Background(), GoProgram(func(*pulumi.Context) error { return errors.New("nope") }), Options{})
	for range op.Events() {
	}
	_, err = op.Wait()
	var pf ProgramFailed
	if !errors.As(err, &pf) || !strings.Contains(pf.Message, "nope") {
		t.Fatalf("got %T %v", err, err)
	}
	op = st.Up(context.Background(), GoProgram(func(*pulumi.Context) error { panic("panicked") }), Options{})
	for range op.Events() {
	}
	_, err = op.Wait()
	if !errors.As(err, &pf) || !strings.Contains(pf.Message, "panicked") {
		t.Fatalf("panic: got %T %v", err, err)
	}
}

// TestOfflineCancel cancels an up whose program is blocked and checks that
// Wait returns Cancelled with the checkpoint written, without any provider.
func TestOfflineCancel(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, offlineSpec(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	program := GoProgram(func(pctx *pulumi.Context) error {
		close(started)
		select {
		case <-pctx.Context().Done():
		case <-release:
		}
		return nil
	})
	cctx, cancel := context.WithCancel(ctx)
	op := st.Up(cctx, program, Options{})
	go func() {
		for range op.Events() {
		}
	}()
	<-started
	cancel()
	// Give the engine a moment to preempt the program; if it cannot (the
	// program is a goroutine we do not own), let it finish so that the
	// engine can wrap up. Either way the outcome must be Cancelled.
	select {
	case <-op.Done():
	case <-time.After(3 * time.Second):
		close(release)
	}
	select {
	case <-op.Done():
	case <-time.After(20 * time.Second):
		t.Fatalf("operation did not finish after cancel")
	}
	res, err := op.Wait()
	if KindOf(err) != KindCancelled || !res.Cancelled {
		t.Fatalf("got %v (%+v)", err, res)
	}
	if _, err := st.Export(ctx); err != nil {
		t.Fatalf("checkpoint unreadable after cancel: %v", err)
	}
	// the stack is still usable
	op = st.Destroy(ctx, nil, Options{})
	for range op.Events() {
	}
	if _, err := op.Wait(); err != nil {
		t.Fatalf("destroy after cancel: %v", err)
	}
}

func TestOfflineConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, offlineSpec(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	slow := GoProgram(func(pctx *pulumi.Context) error {
		close(started)
		select {
		case <-release:
		case <-pctx.Context().Done():
		}
		return nil
	})
	first := st.Up(ctx, slow, Options{})
	go func() {
		for range first.Events() {
		}
	}()
	<-started
	second := st.Up(ctx, GoProgram(func(*pulumi.Context) error { return nil }), Options{})
	for range second.Events() {
	}
	_, err = second.Wait()
	close(release)
	if KindOf(err) != KindConcurrentUpdate {
		t.Errorf("second update: got %v, want ConcurrentUpdate", err)
	}
	if _, err := first.Wait(); err != nil {
		t.Errorf("first update: %v", err)
	}
}

func TestVersion(t *testing.T) {
	if !strings.Contains(Version(), LibraryVersion) {
		t.Errorf("Version() = %q", Version())
	}
}

// helpers to build wire events

func diagnosticEventFor(urn, msg string) apitype.EngineEvent {
	e := diagnosticEvent("error", msg)
	e.DiagnosticEvent.URN = urn
	return e
}

func resOpFailedEvent(urn, typ, op string) apitype.EngineEvent {
	return apitype.EngineEvent{ResOpFailedEvent: &apitype.ResOpFailedEvent{
		Metadata: apitype.StepEventMetadata{Op: apitype.OpType(op), URN: urn, Type: typ},
		Status:   1,
	}}
}
