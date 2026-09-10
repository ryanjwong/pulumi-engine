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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/diy"
	"github.com/pulumi/pulumi/pkg/v3/backend/httpstate"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/util/validation"
	pkgws "github.com/pulumi/pulumi/pkg/v3/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// Stack is an open handle on one stack in one backend. It is safe for
// concurrent use; operations on the same stack are serialised by the
// backend's own locking (a second concurrent update yields ConcurrentUpdate).
type Stack struct {
	spec StackSpec
	sink diag.Sink

	backend backend.Backend
	stack   backend.Stack
	project *workspace.Project
	// root is the working directory for operations without their own dir.
	root string

	// opMu serialises operations started from this handle. Pulumi's DIY
	// backend only detects locks held by other backend instances, so a
	// second operation on the same handle would otherwise race.
	opMu sync.Mutex

	mu           sync.Mutex
	projectStack *workspace.ProjectStack // config + secrets settings
	sm           secrets.Manager
	provider     secrets.Provider
	running      map[*Operation]struct{}
}

// Open connects to the backend named by spec, loads or creates the stack and
// prepares its secrets manager and configuration.
func Open(ctx context.Context, spec StackSpec) (*Stack, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	sink := diag.DefaultSink(io.Discard, io.Discard, diag.FormatOptions{Color: colors.Never})

	root := spec.Project.Dir
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, Unclassified{Err: err}
		}
		root = wd
	}

	project := &workspace.Project{
		Name:    tokens.PackageName(spec.Project.Name),
		Runtime: workspace.NewProjectRuntimeInfo(clientRuntimeName, nil),
	}
	if spec.Project.Description != "" {
		d := spec.Project.Description
		project.Description = &d
	}

	be, err := openBackend(ctx, sink, spec, project)
	if err != nil {
		return nil, err
	}

	ref, err := be.ParseStackReference(spec.Name)
	if err != nil {
		return nil, InvalidSpec{Field: "name", Message: err.Error()}
	}
	bs, err := be.GetStack(ctx, ref)
	if err != nil {
		return nil, classifyBackendError(spec.Name, err)
	}
	if bs == nil {
		if !spec.Create {
			return nil, StackNotFound{Name: spec.Name}
		}
		bs, err = be.CreateStack(ctx, ref, root, nil, nil)
		if err != nil {
			return nil, classifyBackendError(spec.Name, err)
		}
	}

	st := &Stack{
		spec:     spec,
		sink:     sink,
		backend:  be,
		stack:    bs,
		project:  project,
		root:     root,
		provider: specSecretsProvider{passphrase: spec.Secrets.Passphrase, hasPassphrase: spec.Secrets.hasPassphrase()},
		running:  map[*Operation]struct{}{},
	}

	ps, err := st.loadProjectStack()
	if err != nil {
		return nil, err
	}
	sm, err := newSecretsManager(ctx, spec.Secrets, bs, ps)
	if err != nil {
		return nil, classifyBackendError(spec.Name, err)
	}
	st.projectStack, st.sm = ps, sm

	for k, v := range spec.Config {
		if err := st.setConfigLocked(ctx, k, v); err != nil {
			return nil, err
		}
	}
	if err := st.saveProjectStack(); err != nil {
		return nil, err
	}
	return st, nil
}

// openBackend constructs the DIY or HTTP backend for the spec.
func openBackend(ctx context.Context, sink diag.Sink, spec StackSpec, project *workspace.Project) (backend.Backend, error) {
	if isDIYBackend(spec.Backend.URL) {
		be, err := diy.New(ctx, sink, spec.Backend.URL, project)
		if err != nil {
			return nil, Unclassified{Err: fmt.Errorf("opening backend %s: %w", spec.Backend.URL, err)}
		}
		return be, nil
	}
	var be backend.Backend
	err := withSpecCredentials(spec.Backend, func() error {
		var err error
		be, err = httpstate.New(ctx, sink, spec.Backend.URL, project, spec.Backend.Insecure)
		return err
	})
	if err != nil {
		return nil, Unclassified{Err: fmt.Errorf("opening backend %s: %w", spec.Backend.URL, err)}
	}
	return be, nil
}

// credentialsMu serialises the credential hand-off below across the process.
var credentialsMu sync.Mutex

// withSpecCredentials runs open, which constructs an HTTP backend, with the
// spec's token visible to it and nothing else.
//
// Pulumi's HTTP backend has no constructor that takes a token: httpstate.New
// reads workspace.GetAccount(url), which is the credentials file at
// PULUMI_CREDENTIALS_PATH or $PULUMI_HOME/credentials.json, and keeps the
// token it finds there in the backend instance's own client. The library
// therefore hands the token over through that file for the duration of the
// constructor only: under a process-wide lock it records the file's previous
// content, stores the spec's account for the URL, constructs the backend,
// and restores the file exactly (deleting it when it did not exist). The
// backend instance keeps the spec's token; concurrent Open calls with
// different tokens for one URL each get their own. PULUMI_ACCESS_TOKEN is
// never consulted (httpstate.New does not read it). What remains process
// global is the location of the hand-off file; see docs/upstream.md.
func withSpecCredentials(spec BackendSpec, open func() error) error {
	if spec.Token == "" {
		// No token: the stored credentials (a `pulumi login`) apply, as
		// they would for the CLI.
		return open()
	}
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	url := httpstate.ValueOrDefaultURL(pkgws.Instance, spec.URL)
	path := credentialsFilePath()
	before, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("reading stored credentials: %w", readErr)
	}
	if err := workspace.StoreAccount(url, workspace.Account{
		AccessToken: spec.Token,
		Insecure:    spec.Insecure,
	}, false); err != nil {
		return fmt.Errorf("storing credentials for the backend constructor: %w", err)
	}
	openErr := open()
	var restoreErr error
	if readErr != nil {
		restoreErr = os.Remove(path)
	} else {
		restoreErr = os.WriteFile(path, before, 0o600)
	}
	if restoreErr != nil {
		return errors.Join(openErr, fmt.Errorf("restoring stored credentials: %w", restoreErr))
	}
	return openErr
}

// credentialsFilePath is where Pulumi keeps credentials.json:
// $PULUMI_CREDENTIALS_PATH, else $PULUMI_HOME (or ~/.pulumi). Pulumi's own
// resolution is unexported (sdk workspace/creds.go getCredsFilePath).
func credentialsFilePath() string {
	if dir := os.Getenv(workspace.PulumiCredentialsPathEnvVar); dir != "" {
		return filepath.Join(dir, "credentials.json")
	}
	home, err := workspace.GetPulumiHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "credentials.json")
	}
	return filepath.Join(home, "credentials.json")
}

// Spec returns a copy of the spec the stack was opened with (with defaults
// applied).
func (s *Stack) Spec() StackSpec { return s.spec }

// Name returns the stack's fully qualified name as the backend reports it.
func (s *Stack) Name() string { return s.stack.Ref().String() }

// projectStackPath is the Pulumi.<stack>.yaml path when a project dir is set.
func (s *Stack) projectStackPath() string {
	if s.spec.Project.Dir == "" {
		return ""
	}
	return filepath.Join(s.spec.Project.Dir, "Pulumi."+s.stack.Ref().Name().String()+".yaml")
}

func (s *Stack) loadProjectStack() (*workspace.ProjectStack, error) {
	path := s.projectStackPath()
	if path == "" {
		return &workspace.ProjectStack{Config: config.Map{}}, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &workspace.ProjectStack{Config: config.Map{}}, nil
		}
		return nil, Unclassified{Err: err}
	}
	ps, err := workspace.LoadProjectStack(s.sink, s.project, path)
	if err != nil {
		return nil, Unclassified{Err: fmt.Errorf("loading %s: %w", path, err)}
	}
	if ps.Config == nil {
		ps.Config = config.Map{}
	}
	return ps, nil
}

// saveProjectStack persists Pulumi.<stack>.yaml when a project dir is set,
// so that the encryption salt and config survive like they do for the CLI.
func (s *Stack) saveProjectStack() error {
	path := s.projectStackPath()
	if path == "" {
		return nil
	}
	if err := s.projectStack.Save(path); err != nil {
		return Unclassified{Err: fmt.Errorf("saving %s: %w", path, err)}
	}
	return nil
}

// parseConfigKey scopes bare keys to the project namespace.
func (s *Stack) parseConfigKey(key string) (config.Key, error) {
	if !strings.Contains(key, ":") {
		return config.MustMakeKey(s.spec.Project.Name, key), nil
	}
	k, err := config.ParseKey(key)
	if err != nil {
		return config.Key{}, InvalidSpec{Field: "config." + key, Message: err.Error()}
	}
	return k, nil
}

// SetConfig sets one configuration value, encrypting it when Secret is set.
// When the spec has a project dir the value is also written to
// Pulumi.<stack>.yaml, like `pulumi config set` would.
func (s *Stack) SetConfig(ctx context.Context, key string, value ConfigValue) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setConfigLocked(ctx, key, value); err != nil {
		return err
	}
	return s.saveProjectStack()
}

func (s *Stack) setConfigLocked(ctx context.Context, key string, value ConfigValue) error {
	k, err := s.parseConfigKey(key)
	if err != nil {
		return err
	}
	var v config.Value
	switch {
	case value.Secret:
		enc, err := s.sm.Encrypter().EncryptValue(ctx, value.Value)
		if err != nil {
			return Unclassified{Err: fmt.Errorf("encrypting config %s: %w", key, err)}
		}
		if value.Object {
			v = config.NewSecureObjectValue(enc)
		} else {
			v = config.NewSecureValue(enc)
		}
	case value.Object:
		v = config.NewObjectValue(value.Value)
	default:
		v = config.NewValue(value.Value)
	}
	if err := s.projectStack.Config.Set(k, v, false); err != nil {
		return InvalidSpec{Field: "config." + key, Message: err.Error()}
	}
	return nil
}

// GetConfig returns one configuration value, decrypted. Missing keys return
// ok=false.
func (s *Stack) GetConfig(ctx context.Context, key string) (value ConfigValue, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.parseConfigKey(key)
	if err != nil {
		return ConfigValue{}, false, err
	}
	v, has, err := s.projectStack.Config.Get(k, false)
	if err != nil {
		return ConfigValue{}, false, InvalidSpec{Field: "config." + key, Message: err.Error()}
	}
	if !has {
		return ConfigValue{}, false, nil
	}
	plain, err := v.Value(s.sm.Decrypter())
	if err != nil {
		return ConfigValue{}, false, Unclassified{Err: fmt.Errorf("decrypting config %s: %w", key, err)}
	}
	return ConfigValue{Value: plain, Secret: v.Secure(), Object: v.Object()}, true, nil
}

// RemoveConfig deletes one configuration key.
func (s *Stack) RemoveConfig(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.parseConfigKey(key)
	if err != nil {
		return err
	}
	if err := s.projectStack.Config.Remove(k, false); err != nil {
		return InvalidSpec{Field: "config." + key, Message: err.Error()}
	}
	return s.saveProjectStack()
}

// Outputs is the stack's output set. Secret outputs are redacted to
// "[secret]" unless showSecrets is set; SecretKeys lists them either way.
type Outputs struct {
	Values     map[string]any `json:"values"`
	SecretKeys []string       `json:"secretKeys"`
}

// freshStack re-resolves the backend stack handle. Pulumi's stack objects
// cache the snapshot they first loaded (the CLI never needs a second look),
// so anything that reads state after an operation must go through here.
func (s *Stack) freshStack(ctx context.Context) (backend.Stack, error) {
	bs, err := s.backend.GetStack(ctx, s.stack.Ref())
	if err != nil {
		return nil, classifyBackendError(s.spec.Name, err)
	}
	if bs == nil {
		return nil, StackNotFound{Name: s.spec.Name}
	}
	return bs, nil
}

// Outputs returns the stack outputs recorded in the latest checkpoint.
func (s *Stack) Outputs(ctx context.Context, showSecrets bool) (Outputs, error) {
	bs, err := s.freshStack(ctx)
	if err != nil {
		return Outputs{}, err
	}
	snap, err := bs.Snapshot(ctx, s.provider)
	if err != nil {
		return Outputs{}, classifyBackendError(s.spec.Name, err)
	}
	out := Outputs{Values: map[string]any{}, SecretKeys: []string{}}
	if snap == nil {
		return out, nil
	}
	for _, res := range snap.Resources {
		if res.Type != resource.RootStackType || res.Parent != "" {
			continue
		}
		for k, v := range res.Outputs {
			if v.ContainsSecrets() {
				out.SecretKeys = append(out.SecretKeys, string(k))
			}
			out.Values[string(k)] = propertyToJSON(v, showSecrets)
		}
		break
	}
	sort.Strings(out.SecretKeys)
	return out, nil
}

// propertyToJSON converts a resource property value into plain JSON-able Go
// values, redacting secrets unless showSecrets.
func propertyToJSON(v resource.PropertyValue, showSecrets bool) any {
	switch {
	case v.IsNull():
		return nil
	case v.IsBool():
		return v.BoolValue()
	case v.IsNumber():
		return v.NumberValue()
	case v.IsString():
		return v.StringValue()
	case v.IsArray():
		arr := make([]any, 0, len(v.ArrayValue()))
		for _, e := range v.ArrayValue() {
			arr = append(arr, propertyToJSON(e, showSecrets))
		}
		return arr
	case v.IsObject():
		m := map[string]any{}
		for k, e := range v.ObjectValue() {
			m[string(k)] = propertyToJSON(e, showSecrets)
		}
		return m
	case v.IsSecret():
		if !showSecrets {
			return "[secret]"
		}
		return propertyToJSON(v.SecretValue().Element, showSecrets)
	case v.IsOutput():
		o := v.OutputValue()
		if !o.Known {
			return nil
		}
		if o.Secret && !showSecrets {
			return "[secret]"
		}
		return propertyToJSON(o.Element, showSecrets)
	case v.IsAsset():
		return map[string]any{"asset": v.AssetValue().Hash}
	case v.IsArchive():
		return map[string]any{"archive": v.ArchiveValue().Hash}
	case v.IsComputed():
		return nil
	case v.IsResourceReference():
		return string(v.ResourceReferenceValue().URN)
	default:
		return v.String()
	}
}

// Export returns the stack's checkpoint as Pulumi's untyped deployment JSON
// ({"version": 3, "deployment": {...}}), secrets still encrypted.
func (s *Stack) Export(ctx context.Context) (json.RawMessage, error) {
	dep, err := backend.ExportStackDeployment(ctx, s.stack)
	if err != nil {
		return nil, classifyBackendError(s.spec.Name, err)
	}
	if dep == nil {
		dep = &apitype.UntypedDeployment{Version: apitype.DeploymentSchemaVersionCurrent, Deployment: json.RawMessage("{}")}
	}
	return json.Marshal(dep)
}

// Import replaces the stack's checkpoint with the given untyped deployment
// JSON (the format Export returns).
func (s *Stack) Import(ctx context.Context, deployment json.RawMessage) error {
	var dep apitype.UntypedDeployment
	if err := json.Unmarshal(deployment, &dep); err != nil {
		return InvalidSpec{Field: "deployment", Message: err.Error()}
	}
	if dep.Version == 0 || len(dep.Deployment) == 0 {
		return InvalidSpec{Field: "deployment", Message: "expected {\"version\": N, \"deployment\": {...}}"}
	}
	if err := backend.ImportStackDeployment(ctx, s.stack, &dep); err != nil {
		return classifyBackendError(s.spec.Name, err)
	}
	return nil
}

// Remove deletes the stack from the backend. With force false the backend
// refuses when resources remain.
func (s *Stack) Remove(ctx context.Context, force bool) error {
	hasResources, err := backend.RemoveStack(ctx, s.stack, force, false)
	if err != nil {
		return classifyBackendError(s.spec.Name, err)
	}
	if hasResources {
		return Unclassified{Err: fmt.Errorf("stack %q still has resources; destroy them or pass force", s.spec.Name)}
	}
	if path := s.projectStackPath(); path != "" {
		_ = os.Remove(path)
	}
	return nil
}

// Cancel cancels every operation started from this handle and, for backends
// that support it, asks the backend to cancel the stack's current update.
func (s *Stack) Cancel(ctx context.Context) error {
	s.mu.Lock()
	ops := make([]*Operation, 0, len(s.running))
	for op := range s.running {
		ops = append(ops, op)
	}
	s.mu.Unlock()
	for _, op := range ops {
		op.Cancel()
	}
	if s.backend.SupportsProgress() {
		if err := s.backend.CancelCurrentUpdate(ctx, s.stack.Ref()); err != nil {
			return classifyBackendError(s.spec.Name, err)
		}
	}
	return nil
}

// pendingOperationURNs lists the URNs of pending operations in the checkpoint.
func (s *Stack) pendingOperationURNs(ctx context.Context) []string {
	bs, err := s.freshStack(ctx)
	if err != nil {
		return nil
	}
	snap, err := bs.Snapshot(ctx, s.provider)
	if err != nil || snap == nil {
		return nil
	}
	urns := make([]string, 0, len(snap.PendingOperations))
	for _, op := range snap.PendingOperations {
		urns = append(urns, string(op.Resource.URN))
	}
	return urns
}

// Tags, listing and history.

// backendScheme names the backend family for Unsupported errors.
func backendScheme(url string) string {
	if isDIYBackend(url) {
		return "diy"
	}
	return "http"
}

// GetTags returns the stack's tags.
//
// HTTP backends store tags in the service. The DIY backend (Pulumi v3.237.0)
// stores them in a "<stack>.pulumi-tags" JSON file beside the checkpoint,
// which a CLI of the same or a newer version reads (`pulumi stack tag ls`).
func (s *Stack) GetTags(ctx context.Context) (map[string]string, error) {
	bs, err := s.freshStack(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range bs.Tags() {
		out[k] = v
	}
	return out, nil
}

// SetTags replaces the stack's tags with tags (an empty map removes all of
// them). Tag names and values are validated as the service does (name up to
// 40 characters of [a-zA-Z0-9-_.:], value up to 256 characters).
//
// DIY limit (Pulumi v3.237.0): the backend skips the write when the new map
// is empty, so removing the last tag clears it for this handle but leaves
// the tags file as it was for other readers. Set at least one tag, or
// remove the stack, to get rid of the file.
func (s *Stack) SetTags(ctx context.Context, tags map[string]string) error {
	bs, err := s.freshStack(ctx)
	if err != nil {
		return err
	}
	if tags == nil {
		tags = map[string]string{}
	}
	if err := validation.ValidateStackTags(tags); err != nil {
		return InvalidSpec{Field: "tags", Message: err.Error()}
	}
	if err := backend.UpdateStackTags(ctx, bs, tags); err != nil {
		return classifyBackendError(s.spec.Name, err)
	}
	s.mu.Lock()
	s.stack = bs
	s.mu.Unlock()
	return nil
}

// StackSummary is one entry of a stack listing.
type StackSummary struct {
	// Name is the stack reference as the backend renders it for a listing:
	// the organization and project are elided when they are the ones the
	// listing was filtered by ("dev"), as `pulumi stack ls` prints them.
	Name string `json:"name"`
	// FullName is the fully qualified reference ("org/project/dev";
	// "organization/project/dev" on DIY backends).
	FullName string `json:"fullName"`
	// Project is the project the stack belongs to, when the backend knows it.
	Project string `json:"project,omitempty"`
	// LastUpdate is the time of the last update, when the backend knows it.
	LastUpdate *time.Time `json:"lastUpdate,omitempty"`
	// ResourceCount is the number of resources in the checkpoint, when known.
	ResourceCount *int `json:"resourceCount,omitempty"`
}

// ListFilter narrows a stack listing.
type ListFilter struct {
	// Project restricts the listing to one project. On DIY backends this
	// works for the project-scoped state layout (the default since Pulumi
	// 3.x, where checkpoints live under .pulumi/stacks/<project>/); a legacy
	// layout has no project per stack and the filter is a no-op.
	Project string `json:"project,omitempty"`
	// Organization restricts the listing to one organization (HTTP backends
	// only; Unsupported on DIY backends).
	Organization string `json:"organization,omitempty"`
	// TagName/TagValue restrict the listing to stacks carrying the tag.
	TagName  string `json:"tagName,omitempty"`
	TagValue string `json:"tagValue,omitempty"`
}

// ListStacks lists the stacks a backend holds, following continuation
// tokens until the listing is complete.
func ListStacks(ctx context.Context, spec BackendSpec, filter ListFilter) ([]StackSummary, error) {
	stackSpec := StackSpec{Name: "list", Project: ProjectSpec{Name: "list"}, Backend: spec, Secrets: SecretsSpec{Provider: secretsB64}}
	if err := stackSpec.validate(); err != nil {
		return nil, err
	}
	if filter.Organization != "" && isDIYBackend(spec.URL) {
		return nil, Unsupported{Feature: "listStacks.organization", Backend: "diy",
			Message: "DIY backends have no organizations"}
	}
	sink := diag.DefaultSink(io.Discard, io.Discard, diag.FormatOptions{Color: colors.Never})
	var project *workspace.Project
	if filter.Project != "" {
		if _, err := tokens.ParseStackName(filter.Project); err != nil {
			return nil, InvalidSpec{Field: "filter.project", Message: err.Error()}
		}
		project = &workspace.Project{
			Name:    tokens.PackageName(filter.Project),
			Runtime: workspace.NewProjectRuntimeInfo(clientRuntimeName, nil),
		}
	}
	be, err := openBackend(ctx, sink, stackSpec, project)
	if err != nil {
		return nil, err
	}
	f := backend.ListStacksFilter{}
	if filter.Project != "" {
		f.Project = &filter.Project
	}
	if filter.Organization != "" {
		f.Organization = &filter.Organization
	}
	if filter.TagName != "" {
		f.TagName = &filter.TagName
	}
	if filter.TagValue != "" {
		f.TagValue = &filter.TagValue
	}
	out := []StackSummary{}
	var token backend.ContinuationToken
	for {
		summaries, next, err := be.ListStacks(ctx, f, token)
		if err != nil {
			return nil, classifyBackendError("", err)
		}
		for _, sum := range summaries {
			entry := StackSummary{
				Name:          sum.Name().String(),
				FullName:      string(sum.Name().FullyQualifiedName()),
				LastUpdate:    sum.LastUpdate(),
				ResourceCount: sum.ResourceCount(),
			}
			if p, ok := sum.Name().Project(); ok {
				entry.Project = string(p)
			}
			out = append(out, entry)
		}
		if next == nil {
			break
		}
		token = next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out, nil
}

// UpdateInfo is one entry of a stack's update history.
type UpdateInfo struct {
	// Kind is the update kind ("update", "preview", "refresh", "destroy", ...).
	Kind string `json:"kind"`
	// Result is "succeeded", "failed", "in-progress" or "not-started".
	Result string `json:"result"`
	// Message is the message the update was started with.
	Message string `json:"message"`
	// StartTime and EndTime are Unix seconds.
	StartTime int64 `json:"startTime"`
	EndTime   int64 `json:"endTime"`
	// Version is the update's sequence number on HTTP backends; the DIY
	// backend does not number updates and reports 0.
	Version int `json:"version"`
	// Environment is the metadata recorded with the update ("exec.kind", vcs tags).
	Environment map[string]string `json:"environment"`
	// Config is the configuration the update ran with. Secret values are the
	// stored ciphertext unless the history was requested with showSecrets.
	Config map[string]ConfigValue `json:"config"`
	// ResourceChanges counts resource operations by kind.
	ResourceChanges map[string]int `json:"resourceChanges,omitempty"`
}

// HistoryOptions tunes History.
type HistoryOptions struct {
	// Limit is the maximum number of entries (0: every entry the backend
	// returns; the DIY backend returns all, an HTTP backend its default page).
	Limit int `json:"limit,omitempty"`
	// Page selects a page of Limit entries, from 1.
	Page int `json:"page,omitempty"`
	// ShowSecrets decrypts secret config values with the stack's secrets manager.
	ShowSecrets bool `json:"showSecrets,omitempty"`
}

// History returns the stack's updates, newest first.
//
// The DIY backend keeps one "<stack>-<time>.history.json" file per update
// under .pulumi/history/ and pages through them locally; HTTP backends page
// on the server. Previews are not recorded on either.
func (s *Stack) History(ctx context.Context, opts HistoryOptions) ([]UpdateInfo, error) {
	page := opts.Page
	if page < 1 {
		page = 1
	}
	updates, err := s.backend.GetHistory(ctx, s.stack.Ref(), opts.Limit, page)
	if err != nil {
		return nil, classifyBackendError(s.spec.Name, err)
	}
	s.mu.Lock()
	sm := s.sm
	s.mu.Unlock()
	out := make([]UpdateInfo, 0, len(updates))
	for _, u := range updates {
		info := UpdateInfo{
			Kind:        string(u.Kind),
			Result:      string(u.Result),
			Message:     u.Message,
			StartTime:   u.StartTime,
			EndTime:     u.EndTime,
			Version:     u.Version,
			Environment: map[string]string{},
			Config:      map[string]ConfigValue{},
		}
		for k, v := range u.Environment {
			info.Environment[k] = v
		}
		for k, v := range u.Config {
			cv := ConfigValue{Secret: v.Secure(), Object: v.Object()}
			if v.Secure() && opts.ShowSecrets {
				plain, err := v.Value(sm.Decrypter())
				if err != nil {
					return nil, Unclassified{Err: fmt.Errorf("decrypting history config %s: %w", k, err)}
				}
				cv.Value = plain
			} else {
				cv.Value, _ = v.Value(config.NopDecrypter)
			}
			info.Config[k.String()] = cv
		}
		if len(u.ResourceChanges) > 0 {
			info.ResourceChanges = map[string]int{}
			for op, n := range u.ResourceChanges {
				info.ResourceChanges[string(op)] = n
			}
		}
		out = append(out, info)
	}
	return out, nil
}
