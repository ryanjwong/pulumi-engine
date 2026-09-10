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
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pulumi/esc"
	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	pulumiengine "github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy"
	"github.com/pulumi/pulumi/pkg/v3/util/cancel"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Kind is the operation kind.
type Kind string

// Operation kinds.
const (
	KindPreview Kind = "preview"
	KindUp      Kind = "up"
	KindRefresh Kind = "refresh"
	KindDestroy Kind = "destroy"
)

// Options tunes one operation. The zero value is a plain operation.
type Options struct {
	// Parallel is the maximum number of concurrent resource operations
	// (0 means unlimited, like the CLI).
	Parallel int `json:"parallel,omitempty"`
	// Message is recorded in the update history on backends that keep one.
	Message string `json:"message,omitempty"`
	// Targets restricts the operation to these URNs (globs allowed).
	Targets []string `json:"targets,omitempty"`
	// TargetDependents also operates on dependents of Targets.
	TargetDependents bool `json:"targetDependents,omitempty"`
	// Excludes skips these URNs.
	Excludes []string `json:"excludes,omitempty"`
	// Replaces forces replacement of these URNs (up only).
	Replaces []string `json:"replaces,omitempty"`
	// Refresh refreshes state before the update (up and preview).
	Refresh bool `json:"refresh,omitempty"`
	// ContinueOnError keeps going after a resource failure.
	ContinueOnError bool `json:"continueOnError,omitempty"`
	// ShowSecrets leaves secret values unredacted in events and outputs.
	// Refresh and destroy events are always redacted (see README).
	ShowSecrets bool `json:"showSecrets,omitempty"`
	// DryRun turns Refresh and Destroy into previews. Preview ignores it and
	// Up rejects it.
	DryRun bool `json:"dryRun,omitempty"`
	// Env is added to StackSpec.Env for the plugins and language hosts this
	// operation launches (a key set here wins over the stack's).
	Env map[string]string `json:"env,omitempty"`
}

// Result summarises a finished operation. It is populated on failure too,
// as far as the operation got.
type Result struct {
	Kind Kind `json:"kind"`
	// Summary is the engine's summary event, when one was emitted.
	Summary *apitype.SummaryEvent `json:"summary,omitempty"`
	// Changes counts resource operations by kind ("create", "same", ...).
	Changes map[string]int `json:"changes"`
	// Outputs are the stack outputs after a successful up or refresh.
	Outputs *Outputs `json:"outputs,omitempty"`
	// Failures lists every resource operation that failed.
	Failures []ResourceOpFailed `json:"failures,omitempty"`
	// Cancelled is set when the operation was cancelled.
	Cancelled bool `json:"cancelled,omitempty"`
	// Duration is the wall-clock time of the operation.
	Duration time.Duration `json:"duration"`
}

// Operation is an in-flight or finished preview, up, refresh or destroy.
//
// Events must not be required to be drained: Wait returns even if nobody
// reads Events. Events are delivered in engine order. The channel closes
// after the last event, before Wait returns.
type Operation struct {
	kind   Kind
	stack  *Stack
	dryRun bool

	queue  *eventQueue
	events chan Event
	done   chan struct{}

	result Result
	err    error

	cancelRequested atomic.Bool
	cancelCh        chan struct{}
	terminateCh     chan struct{}
	cancelOnce      sync.Once
	terminateOnce   sync.Once

	mu         sync.Mutex
	sequence   int
	summary    *apitype.SummaryEvent
	failures   []ResourceOpFailed
	errorDiags []string
	urnDiags   map[string][]string
	pendingFn  func() []string
}

func newOperation(kind Kind, s *Stack) *Operation {
	op := &Operation{
		kind:        kind,
		stack:       s,
		queue:       newEventQueue(),
		events:      make(chan Event),
		done:        make(chan struct{}),
		cancelCh:    make(chan struct{}),
		terminateCh: make(chan struct{}),
		urnDiags:    map[string][]string{},
	}
	go op.queue.pump(op.events)
	return op
}

// Kind returns the operation kind.
func (o *Operation) Kind() Kind { return o.kind }

// Events streams engine events. The channel is closed after the last event.
func (o *Operation) Events() <-chan Event { return o.events }

// Done is closed when the operation has finished and its checkpoint is
// written.
func (o *Operation) Done() <-chan struct{} { return o.done }

// Wait blocks until the operation finishes and returns its result and error.
// The error is one of the typed errors in this package (see KindOf).
func (o *Operation) Wait() (Result, error) {
	<-o.done
	return o.result, o.err
}

// Cancel requests a graceful cancel: no new steps start, in-flight steps
// finish, the checkpoint is written, and Wait returns Cancelled. A second
// call terminates the engine immediately, which may leave the checkpoint
// behind reality (the same semantics as a second Ctrl-C in the CLI).
func (o *Operation) Cancel() {
	if o.cancelRequested.CompareAndSwap(false, true) {
		o.cancelOnce.Do(func() { close(o.cancelCh) })
		return
	}
	o.terminateOnce.Do(func() { close(o.terminateCh) })
}

// Preview computes the changes an up would make without applying them.
func (s *Stack) Preview(ctx context.Context, program Program, opts Options) *Operation {
	return s.start(ctx, KindPreview, program, opts)
}

// Up runs the program and applies the resulting changes.
func (s *Stack) Up(ctx context.Context, program Program, opts Options) *Operation {
	return s.start(ctx, KindUp, program, opts)
}

// Refresh reconciles the checkpoint with the providers' view of the world.
// program may be nil.
func (s *Stack) Refresh(ctx context.Context, program Program, opts Options) *Operation {
	return s.start(ctx, KindRefresh, program, opts)
}

// Destroy deletes every resource in the stack. program may be nil.
func (s *Stack) Destroy(ctx context.Context, program Program, opts Options) *Operation {
	return s.start(ctx, KindDestroy, program, opts)
}

func (s *Stack) start(ctx context.Context, kind Kind, program Program, opts Options) *Operation {
	op := newOperation(kind, s)
	op.dryRun = opts.DryRun && kind != KindPreview
	op.pendingFn = func() []string { return s.pendingOperationURNs(context.WithoutCancel(ctx)) }
	s.mu.Lock()
	s.running[op] = struct{}{}
	s.mu.Unlock()
	go op.run(ctx, program, opts)
	return op
}

func (o *Operation) finish(start time.Time, err error) {
	o.mu.Lock()
	o.result.Kind = o.kind
	o.result.Summary = o.summary
	o.result.Changes = map[string]int{}
	if o.summary != nil {
		for k, v := range o.summary.ResourceChanges {
			o.result.Changes[string(k)] = v
		}
	}
	o.fillFailureMessages()
	o.result.Failures = append([]ResourceOpFailed(nil), o.failures...)
	o.result.Duration = time.Since(start)
	o.mu.Unlock()

	o.err = o.classifyOperationError(err)
	if _, ok := o.err.(Cancelled); ok {
		o.result.Cancelled = true
	}

	o.stack.mu.Lock()
	delete(o.stack.running, o)
	o.stack.mu.Unlock()

	// The stream ends with the engine's cancel event, as `pulumi --event-log`
	// and therefore the Automation API's stream do: it is the terminator, not
	// a sign that anything was cancelled.
	o.push(eventFromAPI(apitype.EngineEvent{CancelEvent: &apitype.CancelEvent{}}))
	o.queue.close()
	close(o.done)
}

func (o *Operation) run(ctx context.Context, program Program, opts Options) {
	start := time.Now()
	err := o.execute(ctx, program, opts)
	o.finish(start, err)
}

// execute runs the operation and returns the raw error; run classifies it.
func (o *Operation) execute(ctx context.Context, program Program, opts Options) error {
	s := o.stack

	// Context cancellation is turned into a graceful engine cancel; the
	// backend itself runs under a context that cannot be cancelled so the
	// checkpoint write after the cancel is never interrupted.
	stop := context.AfterFunc(ctx, o.Cancel)
	defer stop()
	bctx := context.WithoutCancel(ctx)

	if opts.DryRun && o.kind == KindUp {
		return InvalidSpec{Field: "options.dryRun", Message: "use Preview instead of Up with DryRun"}
	}
	if !s.opMu.TryLock() {
		return ConcurrentUpdate{Err: fmt.Errorf("another %s is running on this stack handle", o.kind)}
	}
	defer s.opMu.Unlock()
	if program == nil {
		if o.kind == KindPreview || o.kind == KindUp {
			return InvalidSpec{Field: "program", Message: "a program is required for preview and up"}
		}
		program = GoProgram(func(*pulumi.Context) error { return nil })
	}

	prep, err := program.prepare(bctx, s)
	if err != nil {
		return err
	}
	defer func() { _ = prep.close() }()

	stackRef := s.stack.Ref().String()
	o.listenStdout(stackRef)
	defer func() {
		flushStdoutCapture()
		o.unlistenStdout(stackRef)
	}()

	proj := *s.project
	if prep.project != nil {
		proj = *prep.project
	}
	proj.Runtime = prep.runtime
	proj.Main = prep.main

	s.mu.Lock()
	cfg := make(config.Map, len(s.projectStack.Config))
	for k, v := range s.projectStack.Config {
		cfg[k] = v
	}
	sm := s.sm
	s.mu.Unlock()

	stackName := s.stack.Ref().Name().String()
	if err := workspace.ValidateStackConfigAndApplyProjectConfig(
		bctx, stackName, &proj, esc.Value{}, cfg, sm.Encrypter(), sm.Decrypter()); err != nil {
		return InvalidSpec{Field: "config", Message: err.Error()}
	}

	// The plugin host: Pulumi's default host on a context of this
	// operation's own, so that provider and language-host launches carry the
	// spec's (and the operation's) environment. See pluginhost.go.
	decrypted, err := cfg.Decrypt(sm.Decrypter())
	if err != nil {
		return Unclassified{Err: fmt.Errorf("decrypting config: %w", err)}
	}
	host, err := newOperationHost(bctx, o, &proj, prep.root, decrypted, mergeEnv(s.spec.Env, opts.Env), opts.ShowSecrets)
	if err != nil {
		return Unclassified{Err: err}
	}
	defer host.close()

	parallel := int32(math.MaxInt32)
	if opts.Parallel > 0 && opts.Parallel < math.MaxInt32 {
		parallel = int32(opts.Parallel)
	}
	engineOpts := pulumiengine.UpdateOptions{
		Host:             host,
		Parallel:         parallel,
		Refresh:          opts.Refresh,
		Targets:          deploy.NewUrnTargets(opts.Targets),
		ReplaceTargets:   deploy.NewUrnTargets(opts.Replaces),
		Excludes:         deploy.NewUrnTargets(opts.Excludes),
		TargetDependents: opts.TargetDependents,
		ContinueOnError:  opts.ContinueOnError,
		ShowSecrets:      opts.ShowSecrets,
		ExecKind:         "auto.inline",
	}
	disp := display.Options{
		Color:               colors.Never,
		Type:                display.DisplayProgress,
		IsInteractive:       false,
		Stdout:              io.Discard,
		Stderr:              io.Discard,
		SuppressPermalink:   true,
		SuppressProgress:    true,
		SuppressTimings:     true,
		ShowSecrets:         opts.ShowSecrets,
		DeterministicOutput: true,
	}

	var sink *eventSink
	if o.kind == KindRefresh || o.kind == KindDestroy {
		sink, err = startEventSink(o.handleEvent)
		if err != nil {
			return Unclassified{Err: err}
		}
		defer func() { _ = sink.Close() }()
		disp.EventLogPath = sink.URL()
	}

	uop := backend.UpdateOperation{
		Proj: &proj,
		Root: prep.root,
		M: &backend.UpdateMetadata{
			Message:     opts.Message,
			Environment: map[string]string{"exec.kind": "auto.inline", "pulumi-engine": Version()},
		},
		Opts: backend.UpdateOptions{
			Engine:      engineOpts,
			Display:     disp,
			AutoApprove: true,
			SkipPreview: true,
			PreviewOnly: opts.DryRun,
		},
		SecretsManager:  sm,
		SecretsProvider: s.provider,
		StackConfiguration: backend.StackConfiguration{
			Config:    cfg,
			Decrypter: sm.Decrypter(),
		},
		Scopes: scopeSource{op: o},
	}

	var opErr error
	switch o.kind {
	case KindPreview, KindUp:
		raw := make(chan pulumiengine.Event)
		forwarded := make(chan struct{})
		go func() {
			defer close(forwarded)
			// Internal events (default provider steps, the refresh steps of
			// an up --refresh) are passed through: that is what the CLI's
			// --event-log (and so the Automation API) carries, and what the
			// loopback sink delivers for refresh and destroy below. Only the
			// CLI's terminal display and `--json` drop them.
			for e := range raw {
				ev, err := convertEngineEvent(e, opts.ShowSecrets)
				if err != nil {
					ev = eventFromAPI(diagnosticEvent("warning", fmt.Sprintf("pulumi-engine: unconvertible engine event: %v", err)))
				}
				o.handleEvent(ev)
			}
		}()
		if o.kind == KindPreview {
			_, _, opErr = backend.PreviewStack(bctx, s.stack, uop, raw)
		} else {
			_, opErr = backend.UpdateStack(bctx, s.stack, uop, raw)
		}
		close(raw)
		<-forwarded
	case KindRefresh:
		_, opErr = backend.RefreshStack(bctx, s.stack, uop)
	case KindDestroy:
		_, opErr = backend.DestroyStack(bctx, s.stack, uop)
	}

	if opErr == nil && (o.kind == KindUp || o.kind == KindRefresh) && !opts.DryRun {
		if out, err := s.Outputs(bctx, opts.ShowSecrets); err == nil {
			o.mu.Lock()
			o.result.Outputs = &out
			o.mu.Unlock()
		}
	}
	return opErr
}

// mergeEnv overlays op on base.
func mergeEnv(base, op map[string]string) map[string]string {
	if len(base) == 0 && len(op) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(op))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range op {
		out[k] = v
	}
	return out
}

// push stamps the event with its sequence number and timestamp (what the
// CLI's display layer does for `--event-log`) and queues it.
func (o *Operation) push(ev Event) {
	o.mu.Lock()
	ev.Sequence = o.sequence
	o.sequence++
	o.mu.Unlock()
	if ev.Timestamp == 0 {
		ev.Timestamp = int(time.Now().Unix())
	}
	o.queue.push(ev)
}

// handleEvent records what error classification needs and queues the event.
func (o *Operation) handleEvent(ev Event) {
	if ev.Type == EventCancel {
		// The engine's cancel event terminates its stream; the operation
		// emits its own terminator once, in finish, after the last event
		// from any source (engine channel, event sink, plugin host, stdout).
		return
	}
	// Strip Pulumi's colour markup ("<{%reset%}>") from human-readable text;
	// the JSON event path does this itself, the channel path does not.
	switch {
	case ev.DiagnosticEvent != nil:
		d := *ev.DiagnosticEvent
		d.Message = colors.Never.Colorize(d.Message)
		d.Prefix = colors.Never.Colorize(d.Prefix)
		d.Color = string(colors.Never)
		ev.DiagnosticEvent = &d
	case ev.StdoutEvent != nil:
		s := *ev.StdoutEvent
		s.Message = colors.Never.Colorize(s.Message)
		s.Color = string(colors.Never)
		ev.StdoutEvent = &s
	case ev.PolicyEvent != nil:
		p := *ev.PolicyEvent
		p.Message = colors.Never.Colorize(p.Message)
		p.Color = string(colors.Never)
		ev.PolicyEvent = &p
	}
	o.mu.Lock()
	switch {
	case ev.SummaryEvent != nil:
		s := *ev.SummaryEvent
		o.summary = &s
	case ev.ResOpFailedEvent != nil:
		m := ev.ResOpFailedEvent.Metadata
		f := ResourceOpFailed{URN: m.URN, Type: m.Type, Op: string(m.Op), Provider: m.Provider}
		o.failures = append(o.failures, f)
	case ev.DiagnosticEvent != nil:
		if msg, ok := isErrorDiagnostic(ev); ok {
			urn := ev.DiagnosticEvent.URN
			o.urnDiags[urn] = append(o.urnDiags[urn], msg)
		}
	}
	o.mu.Unlock()
	o.push(ev)
}

// fillFailureMessages attaches error diagnostics to the resource failures
// they belong to; the rest are program-level errors. Caller holds o.mu.
func (o *Operation) fillFailureMessages() {
	failed := map[string]bool{}
	for i := range o.failures {
		f := &o.failures[i]
		failed[f.URN] = true
		if msgs := o.urnDiags[f.URN]; len(msgs) > 0 {
			f.Message = strings.Join(msgs, "\n")
		}
	}
	o.errorDiags = o.errorDiags[:0]
	for urn, msgs := range o.urnDiags {
		if failed[urn] {
			continue
		}
		o.errorDiags = append(o.errorDiags, msgs...)
	}
}

func (o *Operation) pendingURNs() []string {
	if o.pendingFn == nil {
		return nil
	}
	return o.pendingFn()
}

// diagnosticEvent builds a synthetic diagnostic in Pulumi's wire form.
func diagnosticEvent(severity, message string) apitype.EngineEvent {
	return apitype.EngineEvent{DiagnosticEvent: &apitype.DiagnosticEvent{
		Severity: severity,
		Message:  message + "\n",
		Color:    string(colors.Never),
	}}
}

// scopeSource adapts an Operation's cancel/terminate signals to the
// backend's CancellationScopeSource, replacing the CLI's SIGINT handler.
type scopeSource struct {
	op *Operation
}

func (s scopeSource) NewScope(ctx context.Context, _ chan<- pulumiengine.Event, _ bool) backend.CancellationScope {
	cctx, src := cancel.NewContext(ctx)
	sc := &scope{ctx: cctx, done: make(chan struct{})}
	go func() {
		select {
		case <-s.op.cancelCh:
			src.Cancel()
		case <-sc.done:
			return
		}
		select {
		case <-s.op.terminateCh:
			src.Terminate()
		case <-sc.done:
		}
	}()
	return sc
}

type scope struct {
	ctx  *cancel.Context
	done chan struct{}
	once sync.Once
}

func (s *scope) Context() *cancel.Context { return s.ctx }
func (s *scope) Close()                   { s.once.Do(func() { close(s.done) }) }

// eventQueue is an unbounded FIFO feeding a channel, so that a producer
// (the engine) never blocks on a consumer that is slow or absent.
type eventQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Event
	closed bool
}

func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *eventQueue) push(e Event) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, e)
	}
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *eventQueue) pump(out chan<- Event) {
	defer close(out)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		e := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		out <- e
	}
}
