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

// Copied from pulumi/pulumi (Apache-2.0, Pulumi Corporation).
//
// upstream: github.com/pulumi/pulumi/sdk/v3@v3.237.0 go/auto/stack.go sha256=2d2de5c48e115cd77c52d383781fe9f98141a0ce92ea237e3f2739597db6fe68
// upstream-reason: the in-process LanguageRuntime gRPC server the Go
//   Automation API uses for inline programs is unexported in sdk/go/auto,
//   and importing that package would pull the CLI shell-out machinery in.
// upstream-delete-when: sdk/go/auto (or a new sdk package) exports an
//   in-process language runtime server that does not depend on the CLI.

package upstream

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// upstream-begin: go/auto/stack.go (languageRuntimeServer)
const (
	stateWaiting = iota
	stateRunning
	stateCanceled
	stateFinished
)

// LanguageRuntimeServer is an in-process LanguageRuntime gRPC server that
// runs one Go function per Run request.
type LanguageRuntimeServer struct {
	pulumirpc.UnimplementedLanguageRuntimeServer

	m sync.Mutex
	c *sync.Cond

	fn      pulumi.RunFunc
	address string

	state  int
	cancel chan bool
	done   <-chan error
}

// isNestedInvocation returns true if pulumi.RunWithContext is on the stack.
func isNestedInvocation() bool {
	depth, callers := 0, make([]uintptr, 32)
	for {
		n := runtime.Callers(depth, callers)
		if n == 0 {
			return false
		}
		depth += n

		frames := runtime.CallersFrames(callers)
		for f, more := frames.Next(); more; f, more = frames.Next() {
			if f.Function == "github.com/pulumi/pulumi/sdk/v3/go/pulumi.RunWithContext" {
				return true
			}
		}
	}
}

// StartLanguageRuntimeServer serves fn on a loopback port.
func StartLanguageRuntimeServer(fn pulumi.RunFunc) (*LanguageRuntimeServer, error) {
	if isNestedInvocation() {
		return nil, errors.New("nested stack operations are not supported https://github.com/pulumi/pulumi/issues/5058")
	}

	s := &LanguageRuntimeServer{
		fn:     fn,
		cancel: make(chan bool),
	}
	s.c = sync.NewCond(&s.m)

	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: s.cancel,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterLanguageRuntimeServer(srv, s)
			return nil
		},
		Options: rpcutil.TracingServerInterceptorOptions(nil),
	})
	if err != nil {
		return nil, err
	}
	s.address, s.done = fmt.Sprintf("127.0.0.1:%d", handle.Port), handle.Done
	return s, nil
}

func (s *LanguageRuntimeServer) Close() error {
	s.m.Lock()
	switch s.state {
	case stateCanceled:
		s.m.Unlock()
		return nil
	case stateWaiting:
		// Not started yet; go ahead and cancel
	default:
		for s.state != stateFinished {
			s.c.Wait()
		}
	}
	s.state = stateCanceled
	s.m.Unlock()

	s.cancel <- true
	close(s.cancel)
	return <-s.done
}

func (s *LanguageRuntimeServer) GetRequiredPlugins(ctx context.Context,
	req *pulumirpc.GetRequiredPluginsRequest,
) (*pulumirpc.GetRequiredPluginsResponse, error) {
	return &pulumirpc.GetRequiredPluginsResponse{}, nil
}

func (s *LanguageRuntimeServer) Run(ctx context.Context, req *pulumirpc.RunRequest) (*pulumirpc.RunResponse, error) {
	s.m.Lock()
	if s.state == stateCanceled {
		s.m.Unlock()
		return nil, errors.New("program canceled")
	}
	s.state = stateRunning
	s.m.Unlock()

	defer func() {
		s.m.Lock()
		s.state = stateFinished
		s.m.Unlock()
		s.c.Broadcast()
	}()

	var engineAddress string
	if len(req.Args) > 0 {
		engineAddress = req.Args[0]
	}
	runInfo := pulumi.RunInfo{
		EngineAddr:       engineAddress,
		MonitorAddr:      req.GetMonitorAddress(),
		Config:           req.GetConfig(),
		ConfigSecretKeys: req.GetConfigSecretKeys(),
		Project:          req.GetProject(),
		Stack:            req.GetStack(),
		Parallel:         req.GetParallel(),
		DryRun:           req.GetDryRun(),
		Organization:     req.GetOrganization(),
	}

	pulumiCtx, err := pulumi.NewContext(ctx, runInfo)
	if err != nil {
		return nil, err
	}
	defer pulumiCtx.Close()

	err = func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				if pErr, ok := r.(error); ok {
					err = fmt.Errorf("go inline source runtime error, an unhandled error occurred: %w", pErr)
				} else {
					err = fmt.Errorf("go inline source runtime error, an unhandled panic occurred: %v", r)
				}
			}
		}()

		return pulumi.RunWithContext(pulumiCtx, s.fn)
	}()
	if err != nil {
		return &pulumirpc.RunResponse{Error: err.Error()}, nil
	}
	return &pulumirpc.RunResponse{}, nil
}

func (s *LanguageRuntimeServer) GetPluginInfo(ctx context.Context, req *emptypb.Empty) (*pulumirpc.PluginInfo, error) {
	return &pulumirpc.PluginInfo{
		Version: "1.0.0",
	}, nil
}

func (s *LanguageRuntimeServer) InstallDependencies(
	req *pulumirpc.InstallDependenciesRequest,
	server pulumirpc.LanguageRuntime_InstallDependenciesServer,
) error {
	return nil
}

// upstream-end: go/auto/stack.go

// Address is the "host:port" the server listens on.
func (s *LanguageRuntimeServer) Address() string { return s.address }
