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

//go:build abitest

// Package abitest links the built libpulumi shared library through its
// generated header and drives it the way a C caller would. Build the library
// first (`make lib`), then run `go test -tags abitest ./capi/abitest`.
//
// Go forbids cgo in _test.go files, so the C calls live here as thin
// wrappers and the test drives the wrappers. Every wrapper follows the ABI's
// memory contract: strings from the library are copied and released with
// pulumi_free before returning.
package abitest

/*
#cgo CFLAGS: -I${SRCDIR}/../../build
#cgo LDFLAGS: -L${SRCDIR}/../../build -lpulumi -Wl,-rpath,${SRCDIR}/../../build
#include <stdlib.h>
#include "libpulumi.h"
*/
import "C"

import "unsafe"

// take reads and frees a string returned by the library. ok is false for a
// NULL pointer.
func take(p *C.char) (s string, ok bool) {
	if p == nil {
		return "", false
	}
	s = C.GoString(p)
	C.pulumi_free(p)
	return s, true
}

func Version() string {
	s, _ := take(C.pulumi_version())
	return s
}

func StackOpen(spec string) (handle int64, errJSON string) {
	var cerr *C.char
	cs := C.CString(spec)
	defer C.free(unsafe.Pointer(cs))
	h := C.pulumi_stack_open(cs, &cerr)
	e, _ := take(cerr)
	return int64(h), e
}

// StackOpenNoErr passes a NULL error out-parameter.
func StackOpenNoErr(spec string) int64 {
	cs := C.CString(spec)
	defer C.free(unsafe.Pointer(cs))
	return int64(C.pulumi_stack_open(cs, nil))
}

func StackClose(h int64) int { return int(C.pulumi_stack_close(C.int64_t(h))) }

func OpStart(h int64, request string) (id int64, errJSON string) {
	var cerr *C.char
	cs := C.CString(request)
	defer C.free(unsafe.Pointer(cs))
	r := C.pulumi_op_start(C.int64_t(h), cs, &cerr)
	e, _ := take(cerr)
	return int64(r), e
}

// OpNextEvent returns the event JSON; end is true at end of stream; "" with
// end false means timeout.
func OpNextEvent(id int64, timeoutMs int32) (event string, end bool) {
	s, ok := take(C.pulumi_op_next_event(C.int64_t(id), C.int32_t(timeoutMs)))
	return s, !ok
}

func OpCancel(id int64) int { return int(C.pulumi_op_cancel(C.int64_t(id))) }

func OpWait(id int64) (result string, errJSON string) {
	var cerr *C.char
	r, _ := take(C.pulumi_op_wait(C.int64_t(id), &cerr))
	e, _ := take(cerr)
	return r, e
}

func OpRelease(id int64) int { return int(C.pulumi_op_release(C.int64_t(id))) }

func StackExport(h int64) (dep string, errJSON string) {
	var cerr *C.char
	d, _ := take(C.pulumi_stack_export(C.int64_t(h), &cerr))
	e, _ := take(cerr)
	return d, e
}

func StackImport(h int64, dep string) (rc int, errJSON string) {
	var cerr *C.char
	cs := C.CString(dep)
	defer C.free(unsafe.Pointer(cs))
	r := C.pulumi_stack_import(C.int64_t(h), cs, &cerr)
	e, _ := take(cerr)
	return int(r), e
}

func StackOutputs(h int64, showSecrets bool) (out string, errJSON string) {
	var cerr *C.char
	show := C.int(0)
	if showSecrets {
		show = 1
	}
	o, _ := take(C.pulumi_stack_outputs(C.int64_t(h), show, &cerr))
	e, _ := take(cerr)
	return o, e
}

func StackSetConfig(h int64, key, value string) (rc int, errJSON string) {
	var cerr *C.char
	k, v := C.CString(key), C.CString(value)
	defer C.free(unsafe.Pointer(k))
	defer C.free(unsafe.Pointer(v))
	r := C.pulumi_stack_set_config(C.int64_t(h), k, v, &cerr)
	e, _ := take(cerr)
	return int(r), e
}

func StackGetConfig(h int64, key string) (value string, errJSON string) {
	var cerr *C.char
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	v, _ := take(C.pulumi_stack_get_config(C.int64_t(h), k, &cerr))
	e, _ := take(cerr)
	return v, e
}

func StackRemove(h int64, force bool) (rc int, errJSON string) {
	var cerr *C.char
	f := C.int(0)
	if force {
		f = 1
	}
	r := C.pulumi_stack_remove(C.int64_t(h), f, &cerr)
	e, _ := take(cerr)
	return int(r), e
}

func StackCancel(h int64) (rc int, errJSON string) {
	var cerr *C.char
	r := C.pulumi_stack_cancel(C.int64_t(h), &cerr)
	e, _ := take(cerr)
	return int(r), e
}
