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

// Command envecho is a test resource provider (pulumi-resource-envecho). Its
// one resource type, envecho:index:Echo, has one output, "value": the value
// of the environment variable named by the input "name" as seen by the
// provider process. The integration tests put it on PATH to prove that the
// library launches providers with the spec's environment.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

type provider struct {
	pulumirpc.UnimplementedResourceProviderServer
}

func (p *provider) Handshake(context.Context, *pulumirpc.ProviderHandshakeRequest) (*pulumirpc.ProviderHandshakeResponse, error) {
	return &pulumirpc.ProviderHandshakeResponse{AcceptSecrets: true, AcceptResources: true, AcceptOutputs: true}, nil
}

func (p *provider) GetPluginInfo(context.Context, *emptypb.Empty) (*pulumirpc.PluginInfo, error) {
	return &pulumirpc.PluginInfo{Version: "0.0.1"}, nil
}

func (p *provider) GetSchema(context.Context, *pulumirpc.GetSchemaRequest) (*pulumirpc.GetSchemaResponse, error) {
	return &pulumirpc.GetSchemaResponse{Schema: "{}"}, nil
}

func (p *provider) CheckConfig(_ context.Context, req *pulumirpc.CheckRequest) (*pulumirpc.CheckResponse, error) {
	return &pulumirpc.CheckResponse{Inputs: req.News}, nil
}

func (p *provider) DiffConfig(context.Context, *pulumirpc.DiffRequest) (*pulumirpc.DiffResponse, error) {
	return &pulumirpc.DiffResponse{Changes: pulumirpc.DiffResponse_DIFF_NONE}, nil
}

func (p *provider) Configure(context.Context, *pulumirpc.ConfigureRequest) (*pulumirpc.ConfigureResponse, error) {
	return &pulumirpc.ConfigureResponse{AcceptSecrets: true, SupportsPreview: true, AcceptResources: true, AcceptOutputs: true}, nil
}

func (p *provider) Check(_ context.Context, req *pulumirpc.CheckRequest) (*pulumirpc.CheckResponse, error) {
	return &pulumirpc.CheckResponse{Inputs: req.News}, nil
}

func (p *provider) Diff(_ context.Context, req *pulumirpc.DiffRequest) (*pulumirpc.DiffResponse, error) {
	olds, err := plugin.UnmarshalProperties(req.OldInputs, plugin.MarshalOptions{})
	if err != nil {
		return nil, err
	}
	news, err := plugin.UnmarshalProperties(req.News, plugin.MarshalOptions{})
	if err != nil {
		return nil, err
	}
	if olds.DeepEquals(news) {
		return &pulumirpc.DiffResponse{Changes: pulumirpc.DiffResponse_DIFF_NONE}, nil
	}
	return &pulumirpc.DiffResponse{Changes: pulumirpc.DiffResponse_DIFF_SOME, Replaces: []string{"name"}}, nil
}

func outputs(inputs *structpb.Struct) (*structpb.Struct, error) {
	props, err := plugin.UnmarshalProperties(inputs, plugin.MarshalOptions{})
	if err != nil {
		return nil, err
	}
	name := props["name"].StringValue()
	out := resource.PropertyMap{
		"name":  resource.NewStringProperty(name),
		"value": resource.NewStringProperty(os.Getenv(name)),
		"pid":   resource.NewNumberProperty(float64(os.Getpid())),
	}
	return plugin.MarshalProperties(out, plugin.MarshalOptions{})
}

func (p *provider) Create(_ context.Context, req *pulumirpc.CreateRequest) (*pulumirpc.CreateResponse, error) {
	out, err := outputs(req.Properties)
	if err != nil {
		return nil, err
	}
	if req.Preview {
		return &pulumirpc.CreateResponse{Properties: out}, nil
	}
	return &pulumirpc.CreateResponse{Id: fmt.Sprintf("echo-%d", os.Getpid()), Properties: out}, nil
}

func (p *provider) Read(_ context.Context, req *pulumirpc.ReadRequest) (*pulumirpc.ReadResponse, error) {
	out, err := outputs(req.Inputs)
	if err != nil {
		return nil, err
	}
	return &pulumirpc.ReadResponse{Id: req.Id, Properties: out, Inputs: req.Inputs}, nil
}

func (p *provider) Update(_ context.Context, req *pulumirpc.UpdateRequest) (*pulumirpc.UpdateResponse, error) {
	out, err := outputs(req.News)
	if err != nil {
		return nil, err
	}
	return &pulumirpc.UpdateResponse{Properties: out}, nil
}

func (p *provider) Delete(context.Context, *pulumirpc.DeleteRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (p *provider) Cancel(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func main() {
	cancel := make(chan bool)
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cancel,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterResourceProviderServer(srv, &provider{})
			return nil
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "envecho: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%d\n", handle.Port)
	<-handle.Done
}
