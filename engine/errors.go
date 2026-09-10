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
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/backend/backenderr"
)

// ErrorKind is the stable, language-neutral classification of an error. It
// is what the C ABI and the bindings switch on.
type ErrorKind string

// Error kinds.
const (
	KindInvalidSpec       ErrorKind = "invalidSpec"
	KindResourceOpFailed  ErrorKind = "resourceOpFailed"
	KindProgramFailed     ErrorKind = "programFailed"
	KindConcurrentUpdate  ErrorKind = "concurrentUpdate"
	KindStackNotFound     ErrorKind = "stackNotFound"
	KindStackExists       ErrorKind = "stackExists"
	KindPendingOperations ErrorKind = "pendingOperations"
	KindCancelled         ErrorKind = "cancelled"
	KindUnsupported       ErrorKind = "unsupported"
	KindUnclassified      ErrorKind = "unclassified"
)

// InvalidSpec reports a StackSpec or Options problem detected before any
// backend or engine work starts.
type InvalidSpec struct {
	Field   string
	Message string
}

func (e InvalidSpec) Error() string { return fmt.Sprintf("invalid spec: %s: %s", e.Field, e.Message) }

// ResourceOpFailed reports that a resource operation failed. It carries the
// first failure; Result.Failures has all of them.
type ResourceOpFailed struct {
	URN      string
	Type     string
	Op       string
	Provider string
	Message  string
}

func (e ResourceOpFailed) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "resource operation failed"
	}
	return fmt.Sprintf("%s %s (%s): %s", e.Op, e.Type, e.URN, strings.TrimSpace(msg))
}

// ProgramFailed reports that the Pulumi program itself failed (an exception,
// a non-zero exit, a failed invoke) rather than a resource operation.
type ProgramFailed struct {
	Message string
}

func (e ProgramFailed) Error() string {
	if e.Message == "" {
		return "program failed"
	}
	return "program failed: " + strings.TrimSpace(e.Message)
}

// ConcurrentUpdate reports that another update holds the stack's lock.
type ConcurrentUpdate struct {
	Err error
}

func (e ConcurrentUpdate) Error() string {
	if e.Err == nil {
		return "another update is in progress on this stack"
	}
	return "another update is in progress on this stack: " + e.Err.Error()
}

// Unwrap returns the underlying backend error.
func (e ConcurrentUpdate) Unwrap() error { return e.Err }

// StackNotFound reports that the stack does not exist and Create was false.
type StackNotFound struct {
	Name string
}

func (e StackNotFound) Error() string { return fmt.Sprintf("stack %q not found", e.Name) }

// StackExists reports that stack creation raced with another creator.
type StackExists struct {
	Name string
}

func (e StackExists) Error() string { return fmt.Sprintf("stack %q already exists", e.Name) }

// PendingOperations reports that the checkpoint records operations that were
// interrupted and must be resolved (for example by importing an edited
// checkpoint) before the stack can be updated.
type PendingOperations struct {
	URNs []string
	Err  error
}

func (e PendingOperations) Error() string {
	return fmt.Sprintf("stack has %d pending operation(s): %s", len(e.URNs), e.Err)
}

// Unwrap returns the underlying engine error.
func (e PendingOperations) Unwrap() error { return e.Err }

// Cancelled reports that the operation was cancelled through context
// cancellation or Operation.Cancel. The checkpoint reflects every step that
// completed before the cancel took effect.
type Cancelled struct {
	Operation Kind
}

func (e Cancelled) Error() string { return fmt.Sprintf("%s cancelled", e.Operation) }

// Unsupported reports that the backend (or Pulumi at the pinned version) does
// not support the requested feature. It is returned instead of a panic or a
// silently ignored argument.
type Unsupported struct {
	// Feature names what was asked for ("listStacks.organization").
	Feature string
	// Backend is the backend URL scheme the feature is unsupported on.
	Backend string
	Message string
}

func (e Unsupported) Error() string {
	msg := fmt.Sprintf("%s is not supported", e.Feature)
	if e.Backend != "" {
		msg += " on " + e.Backend + " backends"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Unclassified wraps an error this library could not map to a more specific
// kind. The wrapped error is Pulumi's own.
type Unclassified struct {
	Err error
}

func (e Unclassified) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error.
func (e Unclassified) Unwrap() error { return e.Err }

// KindOf classifies any error returned by this package.
func KindOf(err error) ErrorKind {
	var (
		invalid  InvalidSpec
		resOp    ResourceOpFailed
		program  ProgramFailed
		conc     ConcurrentUpdate
		notFound StackNotFound
		exists   StackExists
		pending  PendingOperations
		canc     Cancelled
		unsup    Unsupported
	)
	switch {
	case errors.As(err, &invalid):
		return KindInvalidSpec
	case errors.As(err, &resOp):
		return KindResourceOpFailed
	case errors.As(err, &program):
		return KindProgramFailed
	case errors.As(err, &conc):
		return KindConcurrentUpdate
	case errors.As(err, &notFound):
		return KindStackNotFound
	case errors.As(err, &exists):
		return KindStackExists
	case errors.As(err, &pending):
		return KindPendingOperations
	case errors.As(err, &canc):
		return KindCancelled
	case errors.As(err, &unsup):
		return KindUnsupported
	default:
		return KindUnclassified
	}
}

// classifyBackendError maps errors from Pulumi's backend packages that can
// occur outside of an engine run (open, export, import, remove).
func classifyBackendError(name string, err error) error {
	if err == nil {
		return nil
	}
	var (
		notFound backenderr.StackNotFoundError
		exists   backenderr.StackAlreadyExistsError
		conflict backenderr.ConflictingUpdateError
	)
	switch {
	case errors.As(err, &notFound):
		return StackNotFound{Name: name}
	case errors.As(err, &exists):
		return StackExists{Name: name}
	case errors.As(err, &conflict):
		return ConcurrentUpdate{Err: err}
	case isLockError(err):
		return ConcurrentUpdate{Err: err}
	}
	return Unclassified{Err: err}
}

// isLockError recognises the DIY backend's lock conflict, which is a plain
// error string in Pulumi.
func isLockError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "the stack is currently locked") ||
		strings.Contains(msg, "conflict: Another update is currently in progress")
}

// classifyOperationError maps the error from an engine run, using what the
// operation observed on the event stream to pick the right kind.
func (o *Operation) classifyOperationError(err error) error {
	if err == nil {
		return nil
	}
	if o.cancelRequested.Load() || errors.Is(err, context.Canceled) {
		return Cancelled{Operation: o.kind}
	}
	var (
		conflict backenderr.ConflictingUpdateError
		canc     backenderr.CancelledError
	)
	switch {
	case errors.As(err, &conflict), isLockError(err):
		return ConcurrentUpdate{Err: err}
	case errors.As(err, &canc):
		return Cancelled{Operation: o.kind}
	case strings.Contains(err.Error(), "pending operations"):
		return PendingOperations{URNs: o.pendingURNs(), Err: err}
	}
	o.mu.Lock()
	failures := append([]ResourceOpFailed(nil), o.failures...)
	diags := append([]string(nil), o.errorDiags...)
	o.mu.Unlock()
	if len(failures) > 0 {
		return failures[0]
	}
	if len(diags) > 0 {
		return ProgramFailed{Message: strings.Join(diags, "\n")}
	}
	return Unclassified{Err: err}
}
