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
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// eventSink is an in-process implementation of Pulumi's Events gRPC service.
//
// Pulumi's Backend.Refresh and Backend.Destroy do not take an event channel
// (only Preview and Update do); their events are only observable through the
// display layer. The display layer can stream engine events to an Events
// gRPC server when display.Options.EventLogPath is "tcp://<addr>": that is
// the path Pulumi built for the Automation API. We host that server on
// loopback and receive the events as they happen. Nothing touches the file
// system and nothing is tailed.
type eventSink struct {
	pulumirpc.UnimplementedEventsServer

	address string
	cancel  chan bool
	done    <-chan error
	onEvent func(Event)
}

func startEventSink(onEvent func(Event)) (*eventSink, error) {
	s := &eventSink{cancel: make(chan bool), onEvent: onEvent}
	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: s.cancel,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterEventsServer(srv, s)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("starting event sink: %w", err)
	}
	s.address, s.done = fmt.Sprintf("127.0.0.1:%d", handle.Port), handle.Done
	return s, nil
}

// URL is the value to put in display.Options.EventLogPath.
func (s *eventSink) URL() string { return "tcp://" + s.address }

func (s *eventSink) Close() error {
	s.cancel <- true
	close(s.cancel)
	return <-s.done
}

// StreamEvents receives JSON-encoded apitype.EngineEvent values.
func (s *eventSink) StreamEvents(stream pulumirpc.Events_StreamEventsServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&emptypb.Empty{})
		}
		if err != nil {
			return err
		}
		ev, err := ParseEventJSON([]byte(req.GetEvent()))
		if err != nil {
			// A malformed event is a bug in the producer; surface it as a
			// diagnostic rather than dropping the stream.
			ev = eventFromAPI(diagnosticEvent("warning", fmt.Sprintf("pulumi-engine: undecodable engine event: %v", err)))
		}
		s.onEvent(ev)
	}
}

// eventJSON encodes an Event the way Pulumi encodes engine events.
func eventJSON(e Event) ([]byte, error) {
	return json.Marshal(e)
}
