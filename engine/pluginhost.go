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
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/hashicorp/go-multierror"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pulumiengine "github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	envutil "github.com/pulumi/pulumi/sdk/v3/go/common/util/env"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"

	"github.com/ryanjwong/pulumi-engine/internal/upstream"
)

// operationHost is the plugin.Host one operation runs with. It is Pulumi's
// default host (built on a plugin.Context of the operation's own, whose diag
// sinks feed the operation's event stream) with two changes:
//
//   - Provider launches get the operation's environment overlay through the
//     env.Env parameter plugin.Host.Provider already has.
//   - Language hosts are launched by the library (upstream.LaunchLanguagePlugin)
//     because the default host launches them with a nil env, and then handed
//     to Pulumi's own client wrapper (plugin.NewLanguageRuntimeClient).
//
// The engine takes it through engine.UpdateOptions.Host. Nothing about the
// process environment is touched.
type operationHost struct {
	plugin.Host

	pctx *plugin.Context
	env  map[string]string

	mu    sync.Mutex
	langs map[string]*launchedLanguageHost
	once  sync.Once
	err   error
}

type launchedLanguageHost struct {
	plug    *plugin.Plugin
	runtime plugin.LanguageRuntime
}

// newOperationHost builds the host for one operation. It returns the pwd
// and main the engine will compute for the same project so that both agree.
func newOperationHost(ctx context.Context, o *Operation, proj *workspace.Project, root string,
	cfg map[config.Key]string, env map[string]string, showSecrets bool,
) (*operationHost, error) {
	projinfo := &pulumiengine.Projinfo{Proj: proj, Root: root}
	pwd, _, err := projinfo.GetPwdMain()
	if err != nil {
		return nil, err
	}
	emit := func(ev pulumiengine.Event) {
		conv, err := convertEngineEvent(ev, showSecrets)
		if err != nil {
			conv = eventFromAPI(diagnosticEvent("warning", fmt.Sprintf("pulumi-engine: unconvertible plugin diagnostic: %v", err)))
		}
		o.handleEvent(conv)
	}
	pctx, err := plugin.NewContextWithRoot(context.WithoutCancel(ctx),
		upstream.NewDiagEventSink(emit, false), upstream.NewDiagEventSink(emit, true),
		nil /*host: the default one, on this context*/, pwd, root, proj.Runtime.Options(),
		false /*disableProviderPreview*/, nil /*tracing span*/, proj.Plugins, proj.GetPackageSpecs(),
		cfg, nil /*debugging*/, schema.NewLoaderServerFromHost)
	if err != nil {
		return nil, fmt.Errorf("creating plugin host: %w", err)
	}
	return &operationHost{Host: pctx.Host, pctx: pctx, env: env, langs: map[string]*launchedLanguageHost{}}, nil
}

// pluginEnv is the environment overlay for a plugin launch: the store Pulumi
// passes (provider env mappings) with the operation's env on top.
func (h *operationHost) pluginEnv(e envutil.Env) envutil.Env {
	if len(h.env) == 0 {
		return e
	}
	merged := envutil.MapStore{}
	if e != nil && e.GetStore() != nil {
		for k, v := range e.GetStore().Values() {
			merged[k] = v
		}
	}
	maps.Copy(merged, h.env)
	return envutil.NewEnv(merged)
}

// Provider launches (or returns the loaded) provider with the operation env.
func (h *operationHost) Provider(descriptor workspace.PluginDescriptor, e envutil.Env) (plugin.Provider, error) {
	return h.Host.Provider(descriptor, h.pluginEnv(e))
}

// LanguageRuntime launches the language host for runtime with the operation
// env. Without an env overlay, and when PULUMI_DEBUG_LANGUAGES asks to attach
// to a running host, the default host does the work.
func (h *operationHost) LanguageRuntime(runtime string) (plugin.LanguageRuntime, error) {
	if len(h.env) == 0 {
		return h.Host.LanguageRuntime(runtime)
	}
	if port, err := plugin.GetLanguageAttachPort(runtime); err != nil || port != nil {
		return h.Host.LanguageRuntime(runtime)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.langs[runtime]; ok {
		return l.runtime, nil
	}
	path, err := workspace.GetPluginPath(h.pctx.Base(), h.pctx.Diag, workspace.PluginDescriptor{
		Name: strings.ReplaceAll(runtime, tokens.QNameDelimiter, "_"),
		Kind: apitype.LanguagePlugin,
	}, h.Host.GetProjectPlugins())
	if err != nil {
		return nil, err
	}
	// The positional argument for the engine address must come last (the
	// default host passes exactly this).
	args := []string{h.Host.ServerAddr()}
	plug, err := upstream.LaunchLanguagePlugin(h.pctx, h.pctx.Pwd, path, runtime, args,
		envutil.NewEnv(envutil.MapStore(h.env)),
		h.Host.AttachDebugger(plugin.DebugSpec{Type: plugin.DebugTypePlugin, Name: runtime}))
	if err != nil {
		return nil, err
	}
	lr := plugin.NewLanguageRuntimeClient(h.pctx, runtime, pulumirpc.NewLanguageRuntimeClient(plug.Conn))
	h.langs[runtime] = &launchedLanguageHost{plug: plug, runtime: lr}
	return lr, nil
}

// EnsurePlugins mirrors the default host's, dispatching through this host so
// that language hosts get the operation env too.
func (h *operationHost) EnsurePlugins(plugins []workspace.PluginDescriptor, kinds plugin.Flags) error {
	var result error
	for _, p := range plugins {
		switch p.Kind {
		case apitype.AnalyzerPlugin:
			if kinds&plugin.AnalyzerPlugins != 0 {
				if _, err := h.Host.Analyzer(tokens.QName(p.Name)); err != nil {
					result = multierror.Append(result, fmt.Errorf("failed to load analyzer plugin %s: %w", p.Name, err))
				}
			}
		case apitype.LanguagePlugin:
			if kinds&plugin.LanguagePlugins != 0 {
				if _, err := h.LanguageRuntime(p.Name); err != nil {
					result = multierror.Append(result, fmt.Errorf("failed to load language plugin %s: %w", p.Name, err))
				}
			}
		case apitype.ResourcePlugin:
			if kinds&plugin.ResourcePlugins != 0 {
				if _, err := h.Provider(p, envutil.NewEnv(envutil.Global)); err != nil {
					result = multierror.Append(result, fmt.Errorf("failed to load resource plugin %s: %w", p.Name, err))
				}
			}
		case apitype.ConverterPlugin, apitype.ToolPlugin:
			result = multierror.Append(result, fmt.Errorf("unexpected plugin kind: %s", p.Kind))
		}
	}
	return result
}

// SignalCancellation forwards the cancel to the default host's plugins and to
// the language hosts launched here.
func (h *operationHost) SignalCancellation() error {
	err := h.Host.SignalCancellation()
	h.mu.Lock()
	langs := make([]*launchedLanguageHost, 0, len(h.langs))
	for _, l := range h.langs {
		langs = append(langs, l)
	}
	h.mu.Unlock()
	for _, l := range langs {
		if cerr := l.runtime.Cancel(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}
	return err
}

// Close stops the language hosts launched here, then the default host (and
// its providers). Idempotent: the engine closes the host when the update
// terminates and the operation closes it again on the way out.
func (h *operationHost) Close() error {
	h.once.Do(func() {
		h.mu.Lock()
		langs := h.langs
		h.langs = map[string]*launchedLanguageHost{}
		h.mu.Unlock()
		var errs []error
		for _, l := range langs {
			if err := l.plug.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if err := h.Host.Close(); err != nil {
			errs = append(errs, err)
		}
		h.err = errors.Join(errs...)
	})
	return h.err
}

// close releases the host and its plugin context.
func (h *operationHost) close() {
	_ = h.Close()
	_ = h.pctx.Close()
}
