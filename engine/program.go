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
	"path/filepath"

	"github.com/blang/semver"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	pkgworkspace "github.com/pulumi/pulumi/pkg/v3/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ryanjwong/pulumi-engine/internal/upstream"
)

// Program is the Pulumi program an operation runs. Three implementations
// exist: GoProgram (in-process Go function), CallbackProgram (a
// LanguageRuntime gRPC server the caller runs) and LocalProgram (a project
// directory executed by the stock language host for its runtime).
type Program interface {
	// prepare returns the project description the engine needs (runtime and
	// entry point) and a cleanup function. The interface is sealed.
	prepare(ctx context.Context, st *Stack) (*preparedProgram, error)
}

// preparedProgram is what an operation needs from a Program.
type preparedProgram struct {
	// runtime replaces the project's runtime section for this operation.
	runtime workspace.ProjectRuntimeInfo
	// root is the directory the program runs in.
	root string
	// main is the optional entry point relative to root.
	main string
	// close releases any resources (in-process servers).
	close func() error
	// project, when non-nil, is the full project loaded from a Pulumi.yaml
	// (LocalProgram). Its plugin/package settings are honoured.
	project *workspace.Project
}

// clientRuntimeName is the runtime the engine understands as "connect to an
// existing LanguageRuntime server at options.address" (the CLI's --client).
const clientRuntimeName = "client"

// GoProgram runs fn in the calling process with the Go SDK. The engine talks
// to it over loopback gRPC exactly as it would to a language host, but no
// subprocess is started: the LanguageRuntime service is a goroutine in this
// process and the resource monitor is the library's own.
func GoProgram(fn func(ctx *pulumi.Context) error) Program {
	return goProgram{fn: fn}
}

type goProgram struct {
	fn pulumi.RunFunc
}

func (p goProgram) prepare(_ context.Context, st *Stack) (*preparedProgram, error) {
	srv, err := upstream.StartLanguageRuntimeServer(p.fn)
	if err != nil {
		return nil, fmt.Errorf("starting in-process language runtime: %w", err)
	}
	return &preparedProgram{
		runtime: workspace.NewProjectRuntimeInfo(clientRuntimeName, map[string]any{"address": srv.Address()}),
		root:    st.root,
		close:   srv.Close,
	}, nil
}

// GoProgramServer is an in-process LanguageRuntime gRPC server running one
// Go function, for use as a CallbackProgram (for example through the C ABI,
// which cannot take a Go function).
type GoProgramServer struct {
	srv *upstream.LanguageRuntimeServer
}

// ServeGoProgram starts a loopback LanguageRuntime server for fn. Close it
// after the operation that used it has finished.
func ServeGoProgram(fn func(ctx *pulumi.Context) error) (*GoProgramServer, error) {
	srv, err := upstream.StartLanguageRuntimeServer(fn)
	if err != nil {
		return nil, err
	}
	return &GoProgramServer{srv: srv}, nil
}

// Address is the "host:port" to pass as CallbackProgram.Address.
func (s *GoProgramServer) Address() string { return s.srv.Address() }

// Close stops the server, waiting for a running program to finish.
func (s *GoProgramServer) Close() error { return s.srv.Close() }

// CallbackProgram connects the engine to a LanguageRuntime gRPC server the
// caller already runs at Address. This is the mechanism behind the CLI's
// hidden --client flag and behind inline Automation API programs in every
// language SDK.
type CallbackProgram struct {
	Address string
}

func (p CallbackProgram) prepare(_ context.Context, st *Stack) (*preparedProgram, error) {
	if p.Address == "" {
		return nil, InvalidSpec{Field: "program.address", Message: "callback program address is required"}
	}
	return &preparedProgram{
		runtime: workspace.NewProjectRuntimeInfo(clientRuntimeName, map[string]any{"address": p.Address}),
		root:    st.root,
		close:   func() error { return nil },
	}, nil
}

// LocalProgram runs the project in Dir with the stock language host plugin
// for its runtime (pulumi-language-<runtime>), which is started as a plugin
// subprocess and downloaded on first use like any other plugin.
type LocalProgram struct {
	Dir string
	// LanguageVersion pins the version of the language host plugin
	// (pulumi-language-<runtime>) that is installed when none is found on
	// PATH or in the plugin cache. Empty installs the latest release. A host
	// already on PATH or in the cache is used regardless, as the CLI would.
	LanguageVersion string `json:"languageVersion,omitempty"`
}

func (p LocalProgram) prepare(ctx context.Context, st *Stack) (*preparedProgram, error) {
	if p.Dir == "" {
		return nil, InvalidSpec{Field: "program.dir", Message: "local program directory is required"}
	}
	dir, err := filepath.Abs(p.Dir)
	if err != nil {
		return nil, InvalidSpec{Field: "program.dir", Message: err.Error()}
	}
	proj, err := workspace.LoadProject(filepath.Join(dir, "Pulumi.yaml"))
	if err != nil {
		return nil, InvalidSpec{Field: "program.dir", Message: fmt.Sprintf("loading Pulumi.yaml: %v", err)}
	}
	if proj.Name.String() != st.spec.Project.Name {
		return nil, InvalidSpec{Field: "program.dir", Message: fmt.Sprintf(
			"Pulumi.yaml project %q does not match the stack's project %q", proj.Name, st.spec.Project.Name)}
	}
	if err := ensureLanguagePlugin(ctx, st.sink, proj.Runtime.Name(), p.LanguageVersion); err != nil {
		return nil, err
	}
	return &preparedProgram{
		runtime: proj.Runtime,
		root:    dir,
		main:    proj.Main,
		close:   func() error { return nil },
		project: proj,
	}, nil
}

// ensureLanguagePlugin installs the language host plugin for runtime if it
// cannot be found on PATH or in the plugin cache. Providers are installed on
// demand by Pulumi's provider registry; language hosts are not, so this is
// the one place the library has to do it.
func ensureLanguagePlugin(ctx context.Context, sink diag.Sink, runtime, version string) error {
	spec := workspace.PluginDescriptor{Kind: apitype.LanguagePlugin, Name: runtime}
	if _, err := workspace.GetPluginPath(ctx, sink, spec, nil); err == nil {
		return nil
	} else {
		var missing *workspace.MissingError
		if !errors.As(err, &missing) {
			return fmt.Errorf("locating language plugin %q: %w", runtime, err)
		}
	}
	if version != "" {
		v, err := semver.ParseTolerant(version)
		if err != nil {
			return InvalidSpec{Field: "program.languageVersion", Message: err.Error()}
		}
		spec.Version = &v
	}
	log := func(sev diag.Severity, msg string) { sink.Logf(sev, diag.RawMessage("", msg)) }
	if _, err := pkgworkspace.InstallPlugin(ctx, spec, log, schema.NewLoaderServerFromHost); err != nil {
		return fmt.Errorf("installing language plugin %q: %w", runtime, err)
	}
	return nil
}
