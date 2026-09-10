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

// Package main is libpulumi: the C ABI over the engine package, built with
// `go build -buildmode=c-shared`. It is handle based, JSON in and JSON out,
// and never calls back into the host.
package main

/*
#include <stdint.h>
#include <stdlib.h>

// libpulumi C ABI.
//
// Memory contract
//   Every `char*` returned by a libpulumi function is allocated by the library
//   and owned by the caller, who must release it with pulumi_free(). This
//   includes pulumi_version() and error strings written through `char** err`.
//   Strings passed *into* the library are borrowed for the duration of the
//   call only. A NULL `err` out-parameter is allowed; the error is then
//   dropped.
//
// Handles
//   pulumi_stack_open returns a stack handle (> 0) that stays valid until
//   pulumi_stack_close. pulumi_op_start returns an operation id (> 0) that
//   stays valid until pulumi_op_release. Both are process-wide integers, safe
//   to use from any thread; the same operation may be polled from one thread
//   and cancelled from another. A handle of 0 means failure and `err` holds a
//   JSON error object.
//
// Errors
//   Error strings are JSON: {"kind": "...", "message": "..."} plus kind
//   specific fields (urn/type/op/provider for resourceOpFailed, urns for
//   pendingOperations, field for invalidSpec, name for stackNotFound and
//   stackExists, operation for cancelled, feature/backend for unsupported,
//   resources [{urn, message}] for planViolation).
//
// Update plans
//   A preview request with options {"savePlan": "/path"} writes the plan
//   file `pulumi preview --save-plan` would; {"generatePlan": true} returns
//   it in the result's "plan" field instead (or as well). An up request with
//   {"plan": "/path"} or {"planJson": {...}} is constrained to that plan and
//   fails with a planViolation error when the program exceeds it.
//
// Events
//   pulumi_op_next_event blocks up to timeout_ms (negative: forever) for the
//   next event. It returns the event JSON, an empty string "" on timeout
//   (still to be freed), or NULL when the stream has ended or the id is
//   unknown. Events are Pulumi's engine event JSON plus a "type" field.
//
// Blocking
//   pulumi_op_next_event and pulumi_op_wait block; everything else returns
//   promptly (stack_open, export, import, outputs may do backend I/O).
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ryanjwong/pulumi-engine/engine"
)

func main() {}

type registry struct {
	mu     sync.Mutex
	next   atomic.Int64
	stacks map[int64]*engine.Stack
	ops    map[int64]*engine.Operation
}

var reg = &registry{stacks: map[int64]*engine.Stack{}, ops: map[int64]*engine.Operation{}}

func (r *registry) stack(h int64) (*engine.Stack, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stacks[h]
	return s, ok
}

func (r *registry) op(id int64) (*engine.Operation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.ops[id]
	return o, ok
}

// errorJSON renders any error in the ABI's error format.
func errorJSON(err error) string {
	m := map[string]any{"kind": string(engine.KindOf(err)), "message": err.Error()}
	var (
		inv  engine.InvalidSpec
		rof  engine.ResourceOpFailed
		pend engine.PendingOperations
		nf   engine.StackNotFound
		ex   engine.StackExists
		canc engine.Cancelled
		uns  engine.Unsupported
		plan engine.PlanViolation
	)
	switch {
	case errors.As(err, &inv):
		m["field"] = inv.Field
		m["message"] = inv.Message
	case errors.As(err, &plan):
		m["resources"] = plan.Resources
	case errors.As(err, &rof):
		m["urn"], m["type"], m["op"], m["provider"] = rof.URN, rof.Type, rof.Op, rof.Provider
		if rof.Message != "" {
			m["message"] = rof.Message
		}
	case errors.As(err, &pend):
		m["urns"] = pend.URNs
	case errors.As(err, &nf):
		m["name"] = nf.Name
	case errors.As(err, &ex):
		m["name"] = ex.Name
	case errors.As(err, &canc):
		m["operation"] = string(canc.Operation)
	case errors.As(err, &uns):
		m["feature"], m["backend"] = uns.Feature, uns.Backend
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func setErr(out **C.char, err error) {
	if out == nil || err == nil {
		return
	}
	*out = C.CString(errorJSON(err))
}

func setErrf(out **C.char, format string, args ...any) {
	setErr(out, engine.InvalidSpec{Field: "request", Message: fmt.Sprintf(format, args...)})
}

func cstr(s string) *C.char { return C.CString(s) }

func gostr(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

// pulumi_version returns a version string ("pulumi-engine X (pulumi vY)").
//
//export pulumi_version
func pulumi_version() *C.char {
	return cstr(engine.Version())
}

// pulumi_free releases memory returned by the library.
//
//export pulumi_free
func pulumi_free(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// pulumi_stack_open opens (and with "create": true creates) a stack from a
// StackSpec JSON object and returns its handle.
//
//export pulumi_stack_open
func pulumi_stack_open(specJSON *C.char, err **C.char) C.int64_t {
	var spec engine.StackSpec
	if e := json.Unmarshal([]byte(gostr(specJSON)), &spec); e != nil {
		setErr(err, engine.InvalidSpec{Field: "spec", Message: e.Error()})
		return 0
	}
	st, e := engine.Open(context.Background(), spec)
	if e != nil {
		setErr(err, e)
		return 0
	}
	h := reg.next.Add(1)
	reg.mu.Lock()
	reg.stacks[h] = st
	reg.mu.Unlock()
	return C.int64_t(h)
}

// pulumi_stack_close releases a stack handle. Operations already started
// keep running. Returns 0, or -1 for an unknown handle.
//
//export pulumi_stack_close
func pulumi_stack_close(h C.int64_t) C.int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, ok := reg.stacks[int64(h)]; !ok {
		return -1
	}
	delete(reg.stacks, int64(h))
	return 0
}

// opRequest is the JSON body of pulumi_op_start.
type opRequest struct {
	Kind    engine.Kind `json:"kind"`
	Program *struct {
		Mode            string `json:"mode"` // "local" | "callback"
		Dir             string `json:"dir,omitempty"`
		Address         string `json:"address,omitempty"`
		LanguageVersion string `json:"languageVersion,omitempty"`
	} `json:"program,omitempty"`
	Options engine.Options `json:"options"`
}

// pulumi_op_start starts a preview, up, refresh or destroy and returns the
// operation id. Request JSON:
//
//	{"kind": "up", "program": {"mode": "local", "dir": "/path"} , "options": {...}}
//	{"kind": "up", "program": {"mode": "callback", "address": "127.0.0.1:1234"}}
//	{"kind": "destroy"}                       (refresh/destroy need no program)
//
//export pulumi_op_start
func pulumi_op_start(h C.int64_t, requestJSON *C.char, err **C.char) C.int64_t {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return 0
	}
	var req opRequest
	if e := json.Unmarshal([]byte(gostr(requestJSON)), &req); e != nil {
		setErr(err, engine.InvalidSpec{Field: "request", Message: e.Error()})
		return 0
	}
	var program engine.Program
	if req.Program != nil {
		switch req.Program.Mode {
		case "local":
			program = engine.LocalProgram{Dir: req.Program.Dir, LanguageVersion: req.Program.LanguageVersion}
		case "callback":
			program = engine.CallbackProgram{Address: req.Program.Address}
		default:
			setErrf(err, "unknown program mode %q (want local or callback)", req.Program.Mode)
			return 0
		}
	}
	ctx := context.Background()
	var op *engine.Operation
	switch req.Kind {
	case engine.KindPreview:
		op = st.Preview(ctx, program, req.Options)
	case engine.KindUp:
		op = st.Up(ctx, program, req.Options)
	case engine.KindRefresh:
		op = st.Refresh(ctx, program, req.Options)
	case engine.KindDestroy:
		op = st.Destroy(ctx, program, req.Options)
	default:
		setErrf(err, "unknown operation kind %q", req.Kind)
		return 0
	}
	id := reg.next.Add(1)
	reg.mu.Lock()
	reg.ops[id] = op
	reg.mu.Unlock()
	return C.int64_t(id)
}

// pulumi_op_next_event returns the next event as JSON, "" on timeout, or
// NULL at end of stream (or for an unknown id).
//
//export pulumi_op_next_event
func pulumi_op_next_event(id C.int64_t, timeoutMs C.int32_t) *C.char {
	op, ok := reg.op(int64(id))
	if !ok {
		return nil
	}
	var timeout <-chan time.Time
	if timeoutMs >= 0 {
		t := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case ev, ok := <-op.Events():
		if !ok {
			return nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			b, _ = json.Marshal(map[string]any{"type": "diagnostic", "diagnosticEvent": map[string]any{
				"severity": "warning", "message": "pulumi-engine: unencodable event: " + err.Error(),
			}})
		}
		return cstr(string(b))
	case <-timeout:
		return cstr("")
	}
}

// pulumi_op_cancel requests a graceful cancel; a second call terminates.
// Returns 0, or -1 for an unknown id.
//
//export pulumi_op_cancel
func pulumi_op_cancel(id C.int64_t) C.int {
	op, ok := reg.op(int64(id))
	if !ok {
		return -1
	}
	op.Cancel()
	return 0
}

// resultJSON is the wire form of engine.Result.
type resultJSON struct {
	engine.Result
	Duration   int64 `json:"duration,omitempty"`
	DurationMs int64 `json:"durationMs"`
}

// pulumi_op_wait blocks until the operation finishes and returns the result
// JSON. On failure it returns NULL and sets err. The result JSON is still
// available through the error's sibling field "result" for failed runs.
//
//export pulumi_op_wait
func pulumi_op_wait(id C.int64_t, err **C.char) *C.char {
	op, ok := reg.op(int64(id))
	if !ok {
		setErrf(err, "unknown operation id %d", int64(id))
		return nil
	}
	res, e := op.Wait()
	wire := resultJSON{Result: res, DurationMs: res.Duration.Milliseconds()}
	b, _ := json.Marshal(wire)
	if e != nil {
		if err != nil {
			var m map[string]any
			_ = json.Unmarshal([]byte(errorJSON(e)), &m)
			m["result"] = json.RawMessage(b)
			eb, _ := json.Marshal(m)
			*err = cstr(string(eb))
		}
		return nil
	}
	return cstr(string(b))
}

// pulumi_op_release forgets an operation id. Call it after pulumi_op_wait.
// Returns 0, or -1 for an unknown id.
//
//export pulumi_op_release
func pulumi_op_release(id C.int64_t) C.int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, ok := reg.ops[int64(id)]; !ok {
		return -1
	}
	delete(reg.ops, int64(id))
	return 0
}

// pulumi_stack_export returns the checkpoint as untyped deployment JSON.
//
//export pulumi_stack_export
func pulumi_stack_export(h C.int64_t, err **C.char) *C.char {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return nil
	}
	dep, e := st.Export(context.Background())
	if e != nil {
		setErr(err, e)
		return nil
	}
	return cstr(string(dep))
}

// pulumi_stack_import replaces the checkpoint. Returns 0 or -1 (err set).
//
//export pulumi_stack_import
func pulumi_stack_import(h C.int64_t, deploymentJSON *C.char, err **C.char) C.int {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return -1
	}
	if e := st.Import(context.Background(), json.RawMessage(gostr(deploymentJSON))); e != nil {
		setErr(err, e)
		return -1
	}
	return 0
}

// pulumi_stack_outputs returns {"values": {...}, "secretKeys": [...]}.
//
//export pulumi_stack_outputs
func pulumi_stack_outputs(h C.int64_t, showSecrets C.int, err **C.char) *C.char {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return nil
	}
	out, e := st.Outputs(context.Background(), showSecrets != 0)
	if e != nil {
		setErr(err, e)
		return nil
	}
	b, _ := json.Marshal(out)
	return cstr(string(b))
}

// pulumi_stack_set_config sets one key from {"value": "...", "secret": bool,
// "object": bool}. Returns 0 or -1 (err set).
//
//export pulumi_stack_set_config
func pulumi_stack_set_config(h C.int64_t, key *C.char, valueJSON *C.char, err **C.char) C.int {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return -1
	}
	var v engine.ConfigValue
	if e := json.Unmarshal([]byte(gostr(valueJSON)), &v); e != nil {
		setErr(err, engine.InvalidSpec{Field: "value", Message: e.Error()})
		return -1
	}
	if e := st.SetConfig(context.Background(), gostr(key), v); e != nil {
		setErr(err, e)
		return -1
	}
	return 0
}

// pulumi_stack_get_config returns {"value": "...", "secret": bool, "object":
// bool} or the JSON literal null when the key is unset.
//
//export pulumi_stack_get_config
func pulumi_stack_get_config(h C.int64_t, key *C.char, err **C.char) *C.char {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return nil
	}
	v, found, e := st.GetConfig(context.Background(), gostr(key))
	if e != nil {
		setErr(err, e)
		return nil
	}
	if !found {
		return cstr("null")
	}
	b, _ := json.Marshal(v)
	return cstr(string(b))
}

// pulumi_stack_remove deletes the stack from the backend. Returns 0 or -1.
//
//export pulumi_stack_remove
func pulumi_stack_remove(h C.int64_t, force C.int, err **C.char) C.int {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return -1
	}
	if e := st.Remove(context.Background(), force != 0); e != nil {
		setErr(err, e)
		return -1
	}
	return 0
}

// pulumi_stack_cancel cancels operations running on the stack (and asks the
// backend to cancel its current update where supported). Returns 0 or -1.
//
//export pulumi_stack_cancel
func pulumi_stack_cancel(h C.int64_t, err **C.char) C.int {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return -1
	}
	if e := st.Cancel(context.Background()); e != nil {
		setErr(err, e)
		return -1
	}
	return 0
}

// pulumi_stack_get_tags returns the stack's tags as a JSON object.
//
//export pulumi_stack_get_tags
func pulumi_stack_get_tags(h C.int64_t, err **C.char) *C.char {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return nil
	}
	tags, e := st.GetTags(context.Background())
	if e != nil {
		setErr(err, e)
		return nil
	}
	b, _ := json.Marshal(tags)
	return cstr(string(b))
}

// pulumi_stack_set_tags replaces the stack's tags with the JSON object
// given. Returns 0 or -1 (err set).
//
//export pulumi_stack_set_tags
func pulumi_stack_set_tags(h C.int64_t, tagsJSON *C.char, err **C.char) C.int {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return -1
	}
	var tags map[string]string
	if e := json.Unmarshal([]byte(gostr(tagsJSON)), &tags); e != nil {
		setErr(err, engine.InvalidSpec{Field: "tags", Message: e.Error()})
		return -1
	}
	if e := st.SetTags(context.Background(), tags); e != nil {
		setErr(err, e)
		return -1
	}
	return 0
}

// pulumi_stack_history returns the stack's update history, newest first, as
// a JSON array of UpdateInfo. Options JSON: {"limit": N, "page": N,
// "showSecrets": bool} (may be empty or NULL).
//
//export pulumi_stack_history
func pulumi_stack_history(h C.int64_t, optionsJSON *C.char, err **C.char) *C.char {
	st, ok := reg.stack(int64(h))
	if !ok {
		setErrf(err, "unknown stack handle %d", int64(h))
		return nil
	}
	var opts engine.HistoryOptions
	if raw := gostr(optionsJSON); raw != "" {
		if e := json.Unmarshal([]byte(raw), &opts); e != nil {
			setErr(err, engine.InvalidSpec{Field: "options", Message: e.Error()})
			return nil
		}
	}
	hist, e := st.History(context.Background(), opts)
	if e != nil {
		setErr(err, e)
		return nil
	}
	b, _ := json.Marshal(hist)
	return cstr(string(b))
}

// pulumi_list_stacks lists the stacks of a backend as a JSON array of
// StackSummary. Request JSON: {"backend": {"url": ..., "token": ...},
// "filter": {"project": ..., "organization": ..., "tagName": ..., "tagValue": ...}}.
//
//export pulumi_list_stacks
func pulumi_list_stacks(requestJSON *C.char, err **C.char) *C.char {
	var req struct {
		Backend engine.BackendSpec `json:"backend"`
		Filter  engine.ListFilter  `json:"filter"`
	}
	if e := json.Unmarshal([]byte(gostr(requestJSON)), &req); e != nil {
		setErr(err, engine.InvalidSpec{Field: "request", Message: e.Error()})
		return nil
	}
	list, e := engine.ListStacks(context.Background(), req.Backend, req.Filter)
	if e != nil {
		setErr(err, e)
		return nil
	}
	b, _ := json.Marshal(list)
	return cstr(string(b))
}
