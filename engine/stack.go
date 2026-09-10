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

	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/diy"
	"github.com/pulumi/pulumi/pkg/v3/backend/httpstate"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
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
		provider: specSecretsProvider{passphrase: spec.Secrets.Passphrase},
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
	if spec.Backend.Token != "" {
		// Pulumi's HTTP backend has no constructor that accepts a token; it
		// reads ~/.pulumi/credentials.json (or PULUMI_CREDENTIALS_PATH). The
		// only way to hand it a token is to store one for this URL. We never
		// change the "current" backend recorded there.
		existing, err := workspace.GetAccount(spec.Backend.URL)
		if err != nil {
			return nil, Unclassified{Err: fmt.Errorf("reading stored credentials: %w", err)}
		}
		if existing.AccessToken != spec.Backend.Token {
			if err := workspace.StoreAccount(spec.Backend.URL, workspace.Account{
				AccessToken: spec.Backend.Token,
				Insecure:    spec.Backend.Insecure,
			}, false); err != nil {
				return nil, Unclassified{Err: fmt.Errorf("storing credentials: %w", err)}
			}
		}
	}
	be, err := httpstate.New(ctx, sink, spec.Backend.URL, project, spec.Backend.Insecure)
	if err != nil {
		return nil, Unclassified{Err: fmt.Errorf("opening backend %s: %w", spec.Backend.URL, err)}
	}
	return be, nil
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
