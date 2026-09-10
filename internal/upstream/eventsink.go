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
// upstream: github.com/pulumi/pulumi/pkg/v3@v3.237.0 engine/eventsink.go sha256=c984ba03f836813eea66ac0c12ff4ea0cb840097618ecb39bba08a7d6181893e
// upstream: github.com/pulumi/pulumi/pkg/v3@v3.237.0 engine/events.go sha256=b867d80c6dab98e18d0f620853ccb36b5fb0bc95fff2cda5172ea940a007c2a2
// upstream-reason: engine.UpdateOptions.Host lets a caller supply the plugin
//   host, but the host must be built on a plugin.Context of its own, and the
//   diag.Sink of that context is where provider and language-host output,
//   plugin warnings and `pulumi.log` calls land. The engine's sink that turns
//   those into diagnostic engine events (newEventSink) is unexported, so it is
//   copied here with the event channel replaced by a callback.
// upstream-delete-when: pkg/engine exports NewEventSink (or UpdateOptions
//   takes a host factory func(*plugin.Context) (plugin.Host, error) so the
//   host can be built on the engine's own context).

package upstream

import (
	"bytes"
	"fmt"

	"github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
)

// NewDiagEventSink returns a diag.Sink that emits every diagnostic as an
// engine.Event (DiagEventPayload) through emit. statusSink marks the events
// ephemeral, as the engine's status sink does.
func NewDiagEventSink(emit func(engine.Event), statusSink bool) diag.Sink {
	return &eventSink{events: emit, statusSink: statusSink}
}

// upstream-begin: engine/events.go (diagEvent)

func diagEvent(emit func(engine.Event), d *diag.Diag, prefix, msg string, sev diag.Severity,
	ephemeral bool,
) {
	emit(engine.NewEvent(engine.DiagEventPayload{
		URN:       d.URN,
		Prefix:    logging.FilterString(prefix),
		Message:   logging.FilterString(msg),
		Color:     colors.Raw,
		Severity:  sev,
		StreamID:  d.StreamID,
		Ephemeral: ephemeral,
	}))
}

// upstream-end: engine/events.go

// upstream-begin: engine/eventsink.go (eventSink)

// eventSink is a sink which writes all events to a channel
type eventSink struct {
	events     func(engine.Event) // the channel to emit events into.
	statusSink bool               // whether this is an event sink for status messages.
}

func (s *eventSink) Logf(sev diag.Severity, d *diag.Diag, args ...any) {
	switch sev {
	case diag.Debug:
		s.Debugf(d, args...)
	case diag.Info:
		s.Infof(d, args...)
	case diag.Infoerr:
		s.Infoerrf(d, args...)
	case diag.Warning:
		s.Warningf(d, args...)
	case diag.Error:
		s.Errorf(d, args...)
	default:
		contract.Failf("Unrecognized severity: %v", sev)
	}
}

func (s *eventSink) Debugf(d *diag.Diag, args ...any) {
	// For debug messages, write both to the glogger and a stream, if there is one.
	logging.V(3).Infof(d.Message, args...)
	prefix, msg := s.Stringify(diag.Debug, d, args...)
	if logging.V(9).Enabled() {
		logging.V(9).Infof("eventSink::Debug(%v)", msg[:len(msg)-1])
	}
	diagEvent(s.events, d, prefix, msg, diag.Debug, s.statusSink)
}

func (s *eventSink) Infof(d *diag.Diag, args ...any) {
	prefix, msg := s.Stringify(diag.Info, d, args...)
	if logging.V(5).Enabled() {
		logging.V(5).Infof("eventSink::Info(%v)", msg[:len(msg)-1])
	}
	diagEvent(s.events, d, prefix, msg, diag.Info, s.statusSink)
}

func (s *eventSink) Infoerrf(d *diag.Diag, args ...any) {
	prefix, msg := s.Stringify(diag.Info /* not Infoerr, just "info: "*/, d, args...)
	if logging.V(5).Enabled() {
		logging.V(5).Infof("eventSink::Infoerr(%v)", msg[:len(msg)-1])
	}
	diagEvent(s.events, d, prefix, msg, diag.Infoerr, s.statusSink)
}

func (s *eventSink) Errorf(d *diag.Diag, args ...any) {
	prefix, msg := s.Stringify(diag.Error, d, args...)
	if logging.V(5).Enabled() {
		logging.V(5).Infof("eventSink::Error(%v)", msg[:len(msg)-1])
	}
	diagEvent(s.events, d, prefix, msg, diag.Error, s.statusSink)
}

func (s *eventSink) Warningf(d *diag.Diag, args ...any) {
	prefix, msg := s.Stringify(diag.Warning, d, args...)
	if logging.V(5).Enabled() {
		logging.V(5).Infof("eventSink::Warning(%v)", msg[:len(msg)-1])
	}
	diagEvent(s.events, d, prefix, msg, diag.Warning, s.statusSink)
}

func (s *eventSink) Stringify(sev diag.Severity, d *diag.Diag, args ...any) (string, string) {
	var prefix bytes.Buffer
	if sev != diag.Info && sev != diag.Infoerr {
		// Unless it's an ordinary stdout message, prepend the message category's prefix (error/warning).
		switch sev {
		case diag.Debug:
			prefix.WriteString(colors.SpecDebug)
		case diag.Error:
			prefix.WriteString(colors.SpecError)
		case diag.Warning:
			prefix.WriteString(colors.SpecWarning)
		case diag.Info, diag.Infoerr:
			// handled above
		default:
			contract.Failf("Unrecognized diagnostic severity: %v", sev)
		}

		prefix.WriteString(string(sev))
		prefix.WriteString(": ")
		prefix.WriteString(colors.Reset)
	}

	// Finally, actually print the message itself.
	var buffer bytes.Buffer
	buffer.WriteString(colors.SpecNote)

	if d.Raw {
		buffer.WriteString(d.Message)
	} else {
		fmt.Fprintf(&buffer, d.Message, args...)
	}

	buffer.WriteString(colors.Reset)
	buffer.WriteRune('\n')

	return prefix.String(), buffer.String()
}

// upstream-end: engine/eventsink.go
