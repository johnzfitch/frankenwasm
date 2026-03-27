package phpext

// #include <stdlib.h>
// #include <stdint.h>
// #cgo CFLAGS: -I../frankenphp
// #include "frankenphp.h"
// #include "phpext.h"
//
import "C"
import (
	"encoding/json"
	"unsafe"

	"github.com/johanjanssens/frankenwasm/wasm"

	"github.com/dunglas/frankenphp"
)

func init() {
	frankenphp.RegisterExtension(unsafe.Pointer(&C.frankenwasm_module_entry))
}

//export go_wasm_list
func go_wasm_list(threadIndex C.uintptr_t) (*C.char, C.bool) {
	thread, ok := frankenphp.Thread(int(threadIndex))
	if !ok || thread.IsRequestDone() {
		return C.CString("Thread not available"), C.bool(false)
	}

	ctx := thread.Request.Context()
	plugins := wasm.FromContext(ctx)
	if plugins == nil {
		return C.CString("No plugin registry in context"), C.bool(false)
	}

	pluginNames := plugins.Names()
	jsonData, err := json.Marshal(pluginNames)
	if err != nil {
		return nil, C.bool(false)
	}

	return C.CString(string(jsonData)), C.bool(true)
}

//export go_wasm_metadata
func go_wasm_metadata(threadIndex C.uintptr_t) (*C.char, C.bool) {
	thread, ok := frankenphp.Thread(int(threadIndex))
	if !ok || thread.IsRequestDone() {
		return C.CString("Thread not available"), C.bool(false)
	}

	ctx := thread.Request.Context()
	plugins := wasm.FromContext(ctx)
	if plugins == nil {
		return C.CString("No plugin registry in context"), C.bool(false)
	}

	metadata := plugins.Metadata()
	jsonData, err := json.Marshal(metadata)
	if err != nil {
		return nil, C.bool(false)
	}

	return C.CString(string(jsonData)), C.bool(true)
}

//export go_wasm_exists
func go_wasm_exists(threadIndex C.uintptr_t, name *C.char) C.bool {
	thread, ok := frankenphp.Thread(int(threadIndex))
	if !ok || thread.IsRequestDone() {
		return C.bool(false)
	}

	ctx := thread.Request.Context()
	plugins := wasm.FromContext(ctx)
	if plugins == nil {
		return C.bool(false)
	}

	return C.bool(plugins.Exists(C.GoString(name)))
}

//export go_wasm_call
func go_wasm_call(threadIndex C.uintptr_t, name *C.char, function *C.char, args *C.char, argsLen C.size_t) (*C.char, C.size_t, C.bool) {
	thread, ok := frankenphp.Thread(int(threadIndex))
	if !ok || thread.IsRequestDone() {
		const msg = "Thread not available"
		return C.CString(msg), C.size_t(len(msg)), C.bool(false)
	}

	ctx := thread.Request.Context()
	plugins := wasm.FromContext(ctx)
	if plugins == nil {
		const msg = "No plugin registry in context"
		return C.CString(msg), C.size_t(len(msg)), C.bool(false)
	}

	// Bounds check: argsLen should not exceed max safe value
	if argsLen > 1<<31-1 {
		const msg = "args length exceeds maximum allowed size"
		return C.CString(msg), C.size_t(len(msg)), C.bool(false)
	}

	// Zero-copy: create a Go []byte view over the C memory.
	// The C args string lives on the PHP stack and is valid for the duration
	// of this CGO call, so this is safe — no copy needed.
	var argsBytes []byte
	if args != nil && argsLen > 0 {
		argsBytes = unsafe.Slice((*byte)(unsafe.Pointer(args)), int(argsLen))
	}

	result, err := plugins.Call(ctx, C.GoString(name), C.GoString(function), argsBytes)

	if err != nil {
		errStr := err.Error()
		return C.CString(errStr), C.size_t(len(errStr)), C.bool(false)
	}

	if result == nil {
		const msg = "failed to call plugin"
		return C.CString(msg), C.size_t(len(msg)), C.bool(false)
	}

	// Zero-copy return: allocate C memory and copy the result bytes directly
	// into it, avoiding the Go string intermediate.
	resultLen := len(result)
	cResult := (*C.char)(C.malloc(C.size_t(resultLen + 1)))
	if cResult == nil {
		const msg = "out of memory"
		return C.CString(msg), C.size_t(len(msg)), C.bool(false)
	}

	// Direct copy from Go []byte to C memory — no intermediate string
	copy(unsafe.Slice((*byte)(unsafe.Pointer(cResult)), resultLen), result)
	// Null-terminate for C string compatibility
	*(*C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(cResult)) + uintptr(resultLen))) = 0

	return cResult, C.size_t(resultLen), C.bool(true)
}
