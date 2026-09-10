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

// Offline tests for the phase-two gaps: empty passphrase, event metadata and
// the cancel terminator, the captured backend banner, tags/listing/history,
// the plugin environment overlay, and HTTP backend credentials against an
// httptest server.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	envutil "github.com/pulumi/pulumi/sdk/v3/go/common/util/env"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestEmptyPassphrase(t *testing.T) {
	ctx := context.Background()
	spec := offlineSpec(t, "dev")
	spec.Secrets = SecretsSpec{Provider: "passphrase"}
	if _, err := Open(ctx, spec); KindOf(err) != KindInvalidSpec {
		t.Fatalf("no passphrase at all should be rejected, got %v", err)
	}

	// JSON: the presence of the key is the confirmation, as for the CLI's
	// PULUMI_CONFIG_PASSPHRASE="".
	var fromJSON StackSpec
	if err := json.Unmarshal([]byte(`{"secrets":{"provider":"passphrase","passphrase":""}}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !fromJSON.Secrets.PassphraseSet || !fromJSON.Secrets.hasPassphrase() {
		t.Fatalf("passphrase key present should set PassphraseSet: %+v", fromJSON.Secrets)
	}
	var absent StackSpec
	_ = json.Unmarshal([]byte(`{"secrets":{"provider":"passphrase"}}`), &absent)
	if absent.Secrets.PassphraseSet {
		t.Fatalf("absent key must not set PassphraseSet")
	}

	spec.Secrets = SecretsSpec{Provider: "passphrase", PassphraseSet: true}
	spec.Config = map[string]ConfigValue{"token": {Value: "s3cret", Secret: true}}
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open with empty passphrase: %v", err)
	}
	op := st.Up(ctx, GoProgram(func(ctx *pulumi.Context) error {
		ctx.Export("out", pulumi.ToSecret(pulumi.String("shh")))
		return nil
	}), Options{})
	for range op.Events() {
	}
	if _, err := op.Wait(); err != nil {
		t.Fatalf("up: %v", err)
	}

	// The same empty passphrase reopens the stack and decrypts; another does not.
	spec.Create = false
	again, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if v, ok, err := again.GetConfig(ctx, "token"); err != nil || !ok || v.Value != "s3cret" {
		t.Fatalf("config after reopen: %+v %v %v", v, ok, err)
	}
	out, err := again.Outputs(ctx, true)
	if err != nil || out.Values["out"] != "shh" {
		t.Fatalf("outputs after reopen: %+v %v", out, err)
	}
	wrong := spec
	wrong.Secrets = SecretsSpec{Provider: "passphrase", Passphrase: "different"}
	if _, err := Open(ctx, wrong); KindOf(err) != KindInvalidSpec {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestEventStreamEnvelope(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, offlineSpec(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	program := GoProgram(func(ctx *pulumi.Context) error { return nil })
	op := st.Up(ctx, program, Options{Message: "hello"})
	var events []Event
	for e := range op.Events() {
		events = append(events, e)
	}
	if _, err := op.Wait(); err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		if e.Sequence != i {
			t.Errorf("event %d has sequence %d", i, e.Sequence)
		}
		if e.Timestamp == 0 {
			t.Errorf("event %d has no timestamp", i)
		}
	}
	last := events[len(events)-1]
	if last.Type != EventCancel || last.CancelEvent == nil {
		t.Errorf("stream should end with the cancel terminator, got %v", eventTypes(events))
	}
	if events[len(events)-2].Type != EventSummary {
		t.Errorf("summary should precede the terminator, got %v", eventTypes(events))
	}
	banner := false
	for _, e := range events {
		if e.StdoutEvent != nil && strings.HasPrefix(e.StdoutEvent.Message, "Updating (dev):") {
			banner = true
		}
	}
	if !banner {
		t.Errorf("the backend banner should arrive as a stdout event: %v", eventTypes(events))
	}
	if os.Stdout == stdoutCapture.orig {
		t.Errorf("os.Stdout should be the capture pipe")
	}

	// An operation that fails before the engine runs still terminates its
	// stream with exactly one cancel event.
	failed := st.Up(ctx, nil, Options{})
	var failedEvents []Event
	for e := range failed.Events() {
		failedEvents = append(failedEvents, e)
	}
	if _, err := failed.Wait(); KindOf(err) != KindInvalidSpec {
		t.Fatalf("expected invalid spec, got %v", err)
	}
	if len(failedEvents) != 1 || failedEvents[0].Type != EventCancel || failedEvents[0].Sequence != 0 {
		t.Errorf("failed operation stream: %v", eventTypes(failedEvents))
	}

	// History records the update; previews are not recorded.
	hist, err := st.History(ctx, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Kind != "update" || hist[0].Result != "succeeded" || hist[0].Message != "hello" ||
		hist[0].StartTime == 0 || hist[0].EndTime < hist[0].StartTime {
		t.Errorf("history: %+v", hist)
	}
	if hist[0].Environment["exec.kind"] != "auto.inline" {
		t.Errorf("history environment: %v", hist[0].Environment)
	}
	if hist[0].ResourceChanges["create"] != 1 {
		t.Errorf("history resource changes: %v", hist[0].ResourceChanges)
	}
}

func TestOfflineTagsAndListing(t *testing.T) {
	ctx := context.Background()
	spec := offlineSpec(t, "dev")
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := st.GetTags(ctx)
	if err != nil || len(tags) != 0 {
		t.Fatalf("fresh stack tags: %v %v", tags, err)
	}
	if err := st.SetTags(ctx, map[string]string{"exa:team": "infra", "exa:journal": "j1"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	tags, err = st.GetTags(ctx)
	if err != nil || tags["exa:team"] != "infra" || tags["exa:journal"] != "j1" {
		t.Fatalf("tags after set: %v %v", tags, err)
	}
	// A fresh handle reads the same tags from the backend.
	other, err := Open(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if tags, _ := other.GetTags(ctx); tags["exa:team"] != "infra" {
		t.Errorf("tags from another handle: %v", tags)
	}
	// Validation follows the service's rules.
	err = st.SetTags(ctx, map[string]string{"bad tag": "x"})
	if KindOf(err) != KindInvalidSpec {
		t.Errorf("invalid tag name: %v", err)
	}
	err = st.SetTags(ctx, map[string]string{"k": strings.Repeat("v", 257)})
	if KindOf(err) != KindInvalidSpec {
		t.Errorf("too long tag value: %v", err)
	}

	// Listing: a second stack in the same project, and one in another.
	spec2 := spec
	spec2.Name = "prod"
	if _, err := Open(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	spec3 := spec
	spec3.Name = "elsewhere"
	spec3.Project.Name = "other"
	spec3.Project.Dir = t.TempDir()
	if _, err := Open(ctx, spec3); err != nil {
		t.Fatal(err)
	}
	all, err := ListStacks(ctx, spec.Backend, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all stacks: %+v", all)
	}
	mine, err := ListStacks(ctx, spec.Backend, ListFilter{Project: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(mine))
	for _, s := range mine {
		names = append(names, s.Name+"="+s.FullName)
		if s.Project != "offline" {
			t.Errorf("project of %s: %q", s.Name, s.Project)
		}
	}
	if strings.Join(names, ",") != "dev=organization/offline/dev,prod=organization/offline/prod" {
		t.Errorf("project listing: %v", names)
	}
	tagged, err := ListStacks(ctx, spec.Backend, ListFilter{TagName: "exa:team", TagValue: "infra"})
	if err != nil || len(tagged) != 1 || !strings.HasSuffix(tagged[0].Name, "/dev") {
		t.Errorf("tag listing: %+v %v", tagged, err)
	}
	_, err = ListStacks(ctx, spec.Backend, ListFilter{Organization: "acme"})
	var unsup Unsupported
	if !errors.As(err, &unsup) || KindOf(err) != KindUnsupported || unsup.Backend != "diy" {
		t.Errorf("organization filter on diy: %v", err)
	}
}

func TestPluginEnvOverlay(t *testing.T) {
	if got := mergeEnv(nil, nil); got != nil {
		t.Errorf("mergeEnv(nil, nil) = %v", got)
	}
	got := mergeEnv(map[string]string{"A": "stack", "B": "stack"}, map[string]string{"B": "op", "C": "op"})
	if got["A"] != "stack" || got["B"] != "op" || got["C"] != "op" {
		t.Errorf("mergeEnv = %v", got)
	}

	h := &operationHost{env: map[string]string{"X": "spec", "PATH": "/spec/bin"}}
	merged := h.pluginEnv(envutil.NewEnv(envutil.MapStore{"X": "pulumi", "Y": "pulumi"}))
	values := map[string]string{}
	for k, v := range merged.GetStore().Values() {
		values[k] = v
	}
	if values["X"] != "spec" || values["Y"] != "pulumi" || values["PATH"] != "/spec/bin" {
		t.Errorf("pluginEnv = %v", values)
	}
	if v, ok := merged.GetStore().Raw("X"); !ok || v != "spec" {
		t.Errorf("Raw(X) = %q %v", v, ok)
	}
	empty := &operationHost{}
	if v, ok := empty.pluginEnv(envutil.NewEnv(envutil.MapStore{"Y": "1"})).GetStore().Raw("Y"); !ok || v != "1" {
		t.Errorf("no overlay should pass the env through")
	}

	// Invalid names are rejected by the spec.
	spec := offlineSpec(t, "dev")
	spec.Env = map[string]string{"BAD=NAME": "x"}
	if _, err := Open(context.Background(), spec); KindOf(err) != KindInvalidSpec {
		t.Errorf("invalid env name: %v", err)
	}
}

func TestStdoutRouting(t *testing.T) {
	st, err := Open(context.Background(), offlineSpec(t, "route"))
	if err != nil {
		t.Fatal(err)
	}
	op := newOperation(KindUp, st)
	ref := st.stack.Ref().String()
	op.listenStdout(ref)
	defer op.unlistenStdout(ref)
	if routeStdoutLine("Previewing update (" + ref + "):\n") {
		t.Errorf("a preview banner must not be routed to an up")
	}
	if routeStdoutLine("something else\n") {
		t.Errorf("unrelated output must pass through")
	}
	if !routeStdoutLine("Updating (" + ref + "):\n") {
		t.Errorf("the up banner should be routed")
	}
	ev := <-op.Events()
	if ev.Type != EventStdout || ev.StdoutEvent.Message != "Updating ("+ref+"):\n" || ev.Sequence != 0 {
		t.Errorf("routed event: %+v", ev)
	}
	if maybeBanner("Upd") != true || maybeBanner("hello") != false || maybeBanner("Updating (x") != true {
		t.Errorf("maybeBanner")
	}
}

// fakeService is the subset of the Pulumi Cloud API a stack open, export,
// tags, history and listing need, recording the Authorization header of
// every request.
type fakeService struct {
	t  *testing.T
	mu sync.Mutex
	// tokens maps an org name to the token requests for its stacks must carry.
	tokens   map[string]string
	requests []string
	tags     map[string]map[string]string
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" "+auth)
	f.mu.Unlock()
	token := strings.TrimPrefix(auth, "token ")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	path := r.URL.Path
	switch {
	case path == "/api/capabilities":
		write(map[string]any{"capabilities": []any{}})
	case path == "/api/user/organizations/default":
		w.WriteHeader(http.StatusNotFound)
	case path == "/api/user/stacks":
		var stacks []map[string]any
		for org, tok := range f.tokens {
			if tok == token && (r.URL.Query().Get("project") == "" || r.URL.Query().Get("project") == "proj") {
				stacks = append(stacks, map[string]any{"orgName": org, "projectName": "proj", "stackName": "dev", "resourceCount": 1})
			}
		}
		write(map[string]any{"stacks": stacks})
	case path == "/api/user":
		for org, tok := range f.tokens {
			if tok == token {
				write(map[string]any{"githubLogin": org, "organizations": []any{}})
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	case strings.HasPrefix(path, "/api/stacks/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/stacks/"), "/")
		org := parts[0]
		if f.tokens[org] != token {
			f.t.Errorf("request for %s carried token %q, want %q", path, token, f.tokens[org])
			w.WriteHeader(http.StatusForbidden)
			return
		}
		rest := strings.Join(parts[3:], "/")
		switch {
		case rest == "" && r.Method == http.MethodGet:
			f.mu.Lock()
			tags := f.tags[org]
			f.mu.Unlock()
			write(map[string]any{"orgName": org, "projectName": "proj", "stackName": "dev", "tags": tags, "version": 1})
		case rest == "export":
			write(map[string]any{"version": 3, "deployment": map[string]any{"manifest": map[string]any{"time": "2026-01-01T00:00:00Z", "magic": "", "version": ""}, "resources": []any{}}})
		case rest == "tags" && r.Method == http.MethodPatch:
			var tags map[string]string
			_ = json.NewDecoder(r.Body).Decode(&tags)
			f.mu.Lock()
			f.tags[org] = tags
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case rest == "updates":
			write(map[string]any{"updates": []map[string]any{{
				"kind": "update", "startTime": 1700000000, "endTime": 1700000010, "message": "m", "version": 7,
				"result": "succeeded", "environment": map[string]string{"exec.kind": "cli"},
				"config":          map[string]any{"proj:k": map[string]any{"string": "v", "secret": false}},
				"resourceChanges": map[string]int{"create": 2},
			}}})
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestHTTPBackendCredentialsPerSpec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PULUMI_HOME", home)
	t.Setenv("PULUMI_ACCESS_TOKEN", "process-token-must-not-be-used")
	svc := &fakeService{t: t, tokens: map[string]string{"alpha": "tok-alpha", "beta": "tok-beta"}, tags: map[string]map[string]string{}}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	// A pre-existing credentials file must survive untouched.
	credsPath := filepath.Join(home, "credentials.json")
	original := `{"current":"https://api.pulumi.com","accessTokens":{"https://api.pulumi.com":"keep-me"}}`
	if err := os.WriteFile(credsPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	open := func(org, token string) (*Stack, error) {
		return Open(ctx, StackSpec{
			Name:    org + "/proj/dev",
			Project: ProjectSpec{Name: "proj"},
			Backend: BackendSpec{URL: srv.URL, Token: token},
		})
	}
	var wg sync.WaitGroup
	stacks := make([]*Stack, 2)
	errs := make([]error, 2)
	for i, org := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stacks[i], errs[i] = open(org, svc.tokens[org])
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	for i, org := range []string{"alpha", "beta"} {
		st := stacks[i]
		if _, err := st.Export(ctx); err != nil {
			t.Errorf("%s export: %v", org, err)
		}
		if err := st.SetTags(ctx, map[string]string{"exa:team": org}); err != nil {
			t.Errorf("%s set tags: %v", org, err)
		}
		if tags, err := st.GetTags(ctx); err != nil || tags["exa:team"] != org {
			t.Errorf("%s tags: %v %v", org, tags, err)
		}
		hist, err := st.History(ctx, HistoryOptions{Limit: 5})
		if err != nil || len(hist) != 1 || hist[0].Version != 7 || hist[0].Config["proj:k"].Value != "v" ||
			hist[0].ResourceChanges["create"] != 2 {
			t.Errorf("%s history: %+v %v", org, hist, err)
		}
	}
	list, err := ListStacks(ctx, BackendSpec{URL: srv.URL, Token: "tok-alpha"}, ListFilter{Project: "proj"})
	if err != nil || len(list) != 1 || list[0].FullName != "alpha/proj/dev" || *list[0].ResourceCount != 1 {
		t.Errorf("list: %+v %v", list, err)
	}

	got, err := os.ReadFile(credsPath)
	if err != nil || string(got) != original {
		t.Errorf("credentials file changed: %q (%v)", got, err)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range svc.requests {
		if strings.Contains(r, "process-token") {
			t.Errorf("process token leaked: %s", r)
		}
		if strings.Contains(r, "?pageSize=5&page=1") {
			seen["history-paging"] = true
		}
		if strings.Contains(r, "tok-alpha") {
			seen["alpha"] = true
		}
		if strings.Contains(r, "tok-beta") {
			seen["beta"] = true
		}
	}
	if !seen["alpha"] || !seen["beta"] || !seen["history-paging"] {
		t.Errorf("requests: %v", svc.requests)
	}
}
