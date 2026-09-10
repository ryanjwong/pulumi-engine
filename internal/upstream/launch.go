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
// upstream: github.com/pulumi/pulumi/sdk/v3@v3.237.0 go/common/resource/plugin/plugin.go sha256=01c4c4a6aca466579a72371969fc069822bd09397358ccf897c554712dd5ed91
// upstream: github.com/pulumi/pulumi/sdk/v3@v3.237.0 go/common/resource/plugin/langruntime_plugin.go sha256=542a18ca53b859e8612061dda221f27cdb45ee15e52bbb27e387da4db0eb9560
// upstream-reason: plugin.Host.Provider takes an env.Env that ExecPlugin
//   appends to the subprocess environment, but NewLanguageRuntime passes a
//   nil env, so a language host launched by the default host always inherits
//   the process environment. To give a language host a per-operation
//   environment the library launches it itself: plugin.ExecPlugin is exported
//   and takes the env, but the part of newPlugin that reads the port line,
//   pumps stdout/stderr into the diag sink and dials the plugin is not, so
//   that part is copied here (without the tracing spans).
// upstream-delete-when: NewLanguageRuntime (or Host.LanguageRuntime) accepts
//   an env.Env like Provider does, or newPlugin is exported.

package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/env"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil/rpcerror"
)

// pluginRPCConnectionTimeout dictates how long we wait for the plugin's RPC to become available.
var pluginRPCConnectionTimeout = time.Second * 10

// nextStreamID numbers the stdout/stderr streams of plugins launched here.
// Pulumi numbers its own from 1 in a package variable we cannot share, so
// ours start high enough never to collide.
var nextStreamID int32 = 1 << 20

// LaunchLanguagePlugin starts the language host binary at bin with the given
// environment overlay, waits for it to announce its port, pumps its stdout and
// stderr into ctx.Diag, and returns the plugin with a ready gRPC connection.
// The caller owns the plugin and must Close it.
func LaunchLanguagePlugin(ctx *plugin.Context, pwd, bin, runtime string, args []string, e env.Env,
	attachDebugger bool,
) (*plugin.Plugin, error) {
	return launchPlugin(ctx, pwd, bin, runtime, apitype.LanguagePlugin, args, e,
		LanguageRuntimeDialOptions(ctx, runtime), attachDebugger)
}

// upstream-begin: go/common/resource/plugin/langruntime_plugin.go (langRuntimePluginDialOptions)

// LanguageRuntimeDialOptions are the gRPC dial options Pulumi uses for a
// language host connection.
func LanguageRuntimeDialOptions(ctx *plugin.Context, runtime string) []grpc.DialOption {
	dialOpts := append(
		rpcutil.TracingInterceptorDialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		rpcutil.GrpcChannelOptions(),
	)

	if ctx.DialOptions != nil {
		metadata := map[string]any{
			"mode": "client",
			"kind": "language",
		}
		if runtime != "" {
			metadata["runtime"] = runtime
		}
		dialOpts = append(dialOpts, ctx.DialOptions(metadata)...)
	}

	return dialOpts
}

// upstream-end: go/common/resource/plugin/langruntime_plugin.go

// upstream-begin: go/common/resource/plugin/plugin.go (newPlugin, parsePort, dialPlugin, testConnection)

func launchPlugin(
	ctx *plugin.Context,
	pwd string,
	bin string,
	prefix string,
	kind apitype.PluginKind,
	args []string,
	e env.Env,
	dialOptions []grpc.DialOption,
	attachDebugger bool,
) (_ *plugin.Plugin, retErr error) {
	// Try to execute the binary.
	plug, err := plugin.ExecPlugin(ctx, bin, prefix, kind, args, pwd, e, attachDebugger)
	if err != nil {
		return nil, fmt.Errorf("failed to load plugin %s: %w", bin, err)
	}
	contract.Assertf(plug != nil, "plugin %v canot be nil", bin)

	// If we did not successfully launch the plugin, we still need to wait for stderr and stdout to drain.
	defer func() {
		if plug.Conn == nil {
			contract.IgnoreClose(plug)
		}
	}()

	type streamID int32
	outStreamID := streamID(atomic.AddInt32(&nextStreamID, 1))
	errStreamID := streamID(atomic.AddInt32(&nextStreamID, 1))

	// For now, we will spawn goroutines that will spew STDOUT/STDERR to the relevant diag streams.
	runtrace := func(t io.Reader, streamID streamID, done chan<- bool) {
		reader := bufio.NewReader(t)

		for {
			msg, readerr := reader.ReadString('\n')

			// Even if we've hit the end of the stream, we want to check for non-empty content.
			// The reason is that if the last line is missing a \n, we still want to include it.
			if strings.TrimSpace(msg) != "" {
				var log func(*diag.Diag, ...any)
				if streamID == outStreamID {
					log = ctx.Diag.Infof
				} else {
					contract.Assertf(streamID == errStreamID, "invalid")
					log = ctx.Diag.Infoerrf
				}
				log(diag.StreamMessage("" /*urn*/, msg, int32(streamID)))
			}

			// If we've hit the end of the stream, break out and close the channel.
			if readerr != nil {
				break
			}
		}

		close(done)
	}

	// Set up a tracer on stderr before going any further, since important errors might get communicated this way.
	stderrDone := make(chan bool)
	go runtrace(plug.Stderr, errStreamID, stderrDone)

	// Now that we have a process, we expect it to write a single line to STDOUT: the port it's listening on.  We only
	// read a byte at a time so that STDOUT contains everything after the first newline.
	var portString string
	b := make([]byte, 1)
	for {
		n, readerr := plug.Stdout.Read(b)
		if readerr != nil {
			killerr := plug.Kill()
			contract.IgnoreError(killerr) // We are ignoring because the readerr trumps it.

			// If readerr is just EOF get the actual error from the plugin.
			if errors.Is(readerr, io.EOF) {
				var exitcode int
				exitcode, readerr = plug.Wait(ctx.Base())
				// If there's no error from waiting, but a non-zero exit code use that as the error.
				if readerr == nil && exitcode != 0 {
					readerr = fmt.Errorf("exit status %d", exitcode)
				}

				// If theres no error from waiting then just report the EOF
				if readerr == nil {
					readerr = io.EOF
				}
			}

			var errMsg string
			detailed, ok := rpcerror.FromError(readerr)
			if ok {
				errMsg = detailed.Error()
			} else {
				errMsg = readerr.Error()
			}
			// Fall back to a generic, opaque error.
			if portString == "" {
				return nil, fmt.Errorf("could not read plugin [%v]: %s", bin, errMsg)
			}
			return nil, fmt.Errorf("failure reading plugin [%v] (read '%v'): %s",
				bin, portString, errMsg)
		}
		if n > 0 && b[0] == '\n' {
			break
		}
		portString += string(b[:n])
	}
	// Parse the output line to ensure it's a numeric port.
	var port int
	if port, err = parsePort(portString); err != nil {
		killerr := plug.Kill()
		contract.IgnoreError(killerr) // ignoring the error because the existing one trumps it.
		return nil, fmt.Errorf(
			"%v plugin [%v] wrote an invalid port to stdout: %w", prefix, bin, err)
	}

	// After reading the port number, set up a tracer on stdout just so other output doesn't disappear.
	stdoutDone := make(chan bool)
	go runtrace(plug.Stdout, outStreamID, stdoutDone)

	conn, err := dialPlugin(ctx.Base(), port, bin, prefix, dialOptions)
	if err != nil {
		return nil, err
	}

	// Done; store the connection and return the plugin info.
	plug.Conn = conn
	return plug, nil
}

func parsePort(portString string) (int, error) {
	// Workaround for https://github.com/dotnet/sdk/issues/44610
	// In .NET 9.0 `dotnet run` will print progress indicators to the terminal,
	// even though it should not do this when the output is redirected.
	// We strip the control characters here to ensure that the port number is parsed correctly.
	//nolint:lll
	// https://github.com/dotnet/sdk/pull/42240/files#diff-6860155f1838e13335d417fc2fed7b13ac5ddf3b95d3548c6646618bc59e89e7R11
	portString = strings.ReplaceAll(portString, "\x1b]9;4;3;\x1b\\", "")
	portString = strings.ReplaceAll(portString, "\x1b]9;4;0;\x1b\\", "")

	// Trim any whitespace from the first line (this is to handle things like windows that will write
	// "1234\r\n", or slightly odd providers that might add whitespace like "1234 ")
	portString = strings.TrimSpace(portString)

	// Parse the output line to ensure it's a numeric port.
	port, err := strconv.Atoi(portString)
	if err != nil {
		// strconv.Atoi already includes the string we tried to parse
		return 0, fmt.Errorf("could not parse port: %w", err)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("invalid port number: %v", port)
	}
	return port, nil
}

func dialPlugin(
	ctx context.Context,
	portNum int,
	bin string,
	prefix string,
	dialOptions []grpc.DialOption,
) (*grpc.ClientConn, error) {
	port := strconv.Itoa(portNum)

	// Now that we have the port, go ahead and create a gRPC client connection to it.
	conn, err := grpc.NewClient("127.0.0.1:"+port, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("could not dial plugin [%v] over RPC: %w", bin, err)
	}

	// We want to wait for the gRPC connection to the plugin to become ready before we proceed. To this end, we'll
	// manually kick off a Connect() call and then wait until the state of the connection becomes Ready.
	conn.Connect()

	// TODO[pulumi/pulumi#337]: in theory, this should be unnecessary.  gRPC's default WaitForReady behavior
	//     should auto-retry appropriately.  On Linux, however, we are observing different behavior.  In the meantime
	//     while this bug exists, we'll simply do a bit of waiting of our own up front.
	timeout, cancel := context.WithTimeout(ctx, pluginRPCConnectionTimeout)
	defer cancel()
	for {
		s := conn.GetState()
		if s == connectivity.Ready {
			// The connection is supposedly ready; but we will make sure it is *actually* ready by sending a dummy
			// method invocation to the server.  Until it responds successfully, we can't safely proceed.
			for {
				err = testConnection(timeout, conn)
				if err == nil {
					break // successful connect
				}

				// We have an error; see if it's a known status and, if so, react appropriately.
				status, ok := status.FromError(err)
				if ok && status.Code() == codes.Unavailable {
					// The server is unavailable.  This is the Linux bug.  Wait a little and retry.
					time.Sleep(time.Millisecond * 10)
					continue // keep retrying
				}

				// Unexpected error; get outta dodge.
				return nil, fmt.Errorf("%v plugin [%v] did not come alive: %w", prefix, bin, err)
			}
			break
		}
		// Not ready yet; ask the gRPC client APIs to block until the state transitions again so we can retry.
		if !conn.WaitForStateChange(timeout, s) {
			return nil, fmt.Errorf("%v plugin [%v] did not begin responding to RPC connections", prefix, bin)
		}
	}

	return conn, nil
}

func testConnection(ctx context.Context, conn *grpc.ClientConn) error {
	err := conn.Invoke(ctx, "", nil, nil)
	if err != nil {
		status, ok := status.FromError(err)
		if ok && status.Code() != codes.Unavailable {
			return nil
		}
	}
	return err
}

// upstream-end: go/common/resource/plugin/plugin.go
