package wasm

import (
	"log/slog"

	extism "github.com/extism/go-sdk"
)

type ManagerOption func(*Manager)

// WithCachePath sets the directory path for the persistent WebAssembly
// compilation cache. Compiled native code survives process restarts.
func WithCachePath(cachePath string) ManagerOption {
	return func(m *Manager) {
		m.cachePath = cachePath
	}
}

// WithLogger sets a custom logger for the plugin manager.
func WithLogger(logger slog.Handler) ManagerOption {
	return func(m *Manager) {
		m.logger = slog.New(logger)
	}
}

// WithHostFunctions adds host functions to the plugin manager.
func WithHostFunctions(functions ...extism.HostFunction) ManagerOption {
	return func(m *Manager) {
		m.hostFunctions = append(m.hostFunctions, functions...)
	}
}

// WithCompilationWorkers sets the number of parallel compilation workers.
// Defaults to 0 (sequential). Set to runtime.NumCPU() for maximum throughput.
func WithCompilationWorkers(n int) ManagerOption {
	return func(m *Manager) {
		m.numWorkers = n
	}
}

// WithSharedRuntime enables a single shared Wazero runtime across all plugins.
// This reduces memory footprint by sharing compiled trampolines, entry preambles,
// and the compilation cache engine. When disabled (default), each CompiledPlugin
// gets its own runtime.
func WithSharedRuntime() ManagerOption {
	return func(m *Manager) {
		m.sharedRuntime = true
	}
}
