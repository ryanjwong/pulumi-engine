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
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	pulumiengine "github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// EventType names the kind of an Event. The names mirror Pulumi's engine
// event JSON (the `--json` / event-log format) with the "Event" suffix
// dropped.
type EventType string

// Event types.
const (
	EventCancel                    EventType = "cancel"
	EventStdout                    EventType = "stdout"
	EventDiagnostic                EventType = "diagnostic"
	EventPrelude                   EventType = "prelude"
	EventSummary                   EventType = "summary"
	EventResourcePre               EventType = "resourcePre"
	EventResourceOutputs           EventType = "resourceOutputs"
	EventResourceOpFailed          EventType = "resourceOpFailed"
	EventPolicy                    EventType = "policy"
	EventPolicyRemediation         EventType = "policyRemediation"
	EventPolicyLoad                EventType = "policyLoad"
	EventPolicyAnalyzeSummary      EventType = "policyAnalyzeSummary"
	EventPolicyRemediateSummary    EventType = "policyRemediateSummary"
	EventPolicyAnalyzeStackSummary EventType = "policyAnalyzeStackSummary"
	EventStartDebugging            EventType = "startDebugging"
	EventProgress                  EventType = "progress"
	EventError                     EventType = "error"
	EventUnknown                   EventType = "unknown"
)

// Event is one engine event. It embeds Pulumi's wire representation
// (apitype.EngineEvent), so exactly one of the *Event pointer fields is set,
// and adds Type so consumers do not have to probe the pointers. Property
// values are secret-redacted ("[secret]") unless Options.ShowSecrets was set.
//
// The JSON form is Pulumi's engine event JSON plus a "type" field.
type Event struct {
	Type EventType `json:"type"`
	apitype.EngineEvent
}

// eventFromAPI wraps an apitype event.
func eventFromAPI(e apitype.EngineEvent) Event {
	return Event{Type: classifyEvent(e), EngineEvent: e}
}

// classifyEvent derives the Type from which payload pointer is set.
func classifyEvent(e apitype.EngineEvent) EventType {
	switch {
	case e.CancelEvent != nil:
		return EventCancel
	case e.StdoutEvent != nil:
		return EventStdout
	case e.DiagnosticEvent != nil:
		return EventDiagnostic
	case e.PreludeEvent != nil:
		return EventPrelude
	case e.SummaryEvent != nil:
		return EventSummary
	case e.ResourcePreEvent != nil:
		return EventResourcePre
	case e.ResOutputsEvent != nil:
		return EventResourceOutputs
	case e.ResOpFailedEvent != nil:
		return EventResourceOpFailed
	case e.PolicyEvent != nil:
		return EventPolicy
	case e.PolicyRemediationEvent != nil:
		return EventPolicyRemediation
	case e.PolicyLoadEvent != nil:
		return EventPolicyLoad
	case e.PolicyAnalyzeSummaryEvent != nil:
		return EventPolicyAnalyzeSummary
	case e.PolicyRemediateSummaryEvent != nil:
		return EventPolicyRemediateSummary
	case e.PolicyAnalyzeStackSummaryEvent != nil:
		return EventPolicyAnalyzeStackSummary
	case e.StartDebuggingEvent != nil:
		return EventStartDebugging
	case e.ProgressEvent != nil:
		return EventProgress
	case e.ErrorEvent != nil:
		return EventError
	default:
		return EventUnknown
	}
}

// convertEngineEvent translates a raw engine event into the wire form. This
// is the same translation the CLI applies for `--json` and event logs; with
// showSecrets false secret property values become "[secret]".
func convertEngineEvent(e pulumiengine.Event, showSecrets bool) (Event, error) {
	api, err := display.ConvertEngineEvent(e, showSecrets)
	if err != nil {
		return Event{}, err
	}
	return eventFromAPI(api), nil
}

// ParseEventJSON decodes one event from Pulumi's engine event JSON (as
// written by `pulumi --json`, `--event-log`, or Event.MarshalJSON). The
// "type" field is optional; it is recomputed from the payload.
func ParseEventJSON(data []byte) (Event, error) {
	var api apitype.EngineEvent
	if err := json.Unmarshal(data, &api); err != nil {
		return Event{}, err
	}
	return eventFromAPI(api), nil
}

// isErrorDiagnostic reports whether e is an error-severity diagnostic and
// returns its message.
func isErrorDiagnostic(e Event) (string, bool) {
	if e.DiagnosticEvent == nil {
		return "", false
	}
	if e.DiagnosticEvent.Severity != "error" {
		return "", false
	}
	msg := strings.TrimSpace(e.DiagnosticEvent.Message)
	if msg == "" {
		return "", false
	}
	return msg, true
}
