// Package wasm provides WebAssembly plugin management using the Extism SDK.
// It handles plugin loading, compilation caching, per-request instantiation,
// and thread-safe concurrent access.
package wasm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"
	"golang.org/x/sync/errgroup"
)

type (
	pluginEntry struct {
		name     string
		plugin   *extism.CompiledPlugin
		fileSize int64
	}

	// Manager handles WebAssembly plugin lifecycle including loading, instantiation,
	// and resource management.
	Manager struct {
		cachePath     string
		logger        *slog.Logger
		hostFunctions []extism.HostFunction
		numWorkers    int // parallel compilation workers (0 = sequential)
		sharedRuntime bool

		plugins       []pluginEntry
		pluginMap     map[string]int // name -> index in plugins slice
		runtimeConfig wazero.RuntimeConfig
		runtimeCache  wazero.CompilationCache
		sr            *extism.SharedRuntime
		mu            sync.RWMutex
		cacheInit     sync.Once
	}
)

// NewManager creates a new plugin manager with the provided options.
func NewManager(opts ...ManagerOption) (*Manager, error) {
	manager := &Manager{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		hostFunctions: []extism.HostFunction{},
		plugins:       make([]pluginEntry, 0),
		pluginMap:     make(map[string]int),
	}

	for _, opt := range opts {
		opt(manager)
	}

	manager.runtimeConfig = wazero.NewRuntimeConfig()

	return manager, nil
}

// Load compiles a WebAssembly plugin from the specified path and
// registers it with the given name.
func (m *Manager) Load(ctx context.Context, name string, path string) error {
	start := time.Now()

	if err := m.initCache(); err != nil {
		return fmt.Errorf("failed to initialize compilation cache: %v", err)
	}

	pluginManifest := extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmFile{Path: path, Name: name}},
		AllowedHosts: []string{"*"},
		Config: map[string]string{
			"name": name,
			"path": path,
		},
	}

	pluginConfig := extism.PluginConfig{
		EnableWasi:    true,
		ModuleConfig:  wazero.NewModuleConfig(),
		RuntimeConfig: m.runtimeConfig,
	}

	var compiledPlugin *extism.CompiledPlugin
	var err error
	if m.sr != nil {
		compiledPlugin, err = extism.NewCompiledPluginWithRuntime(ctx, pluginManifest, pluginConfig, m.hostFunctions, m.sr)
	} else {
		compiledPlugin, err = extism.NewCompiledPlugin(ctx, pluginManifest, pluginConfig, m.hostFunctions)
	}
	if err != nil {
		return fmt.Errorf("failed to compile plugin %s: %v", path, err)
	}

	var fileSize int64
	if info, err := os.Stat(path); err == nil {
		fileSize = info.Size()
	}

	m.mu.Lock()
	if idx, exists := m.pluginMap[name]; exists {
		m.plugins[idx].plugin = compiledPlugin
		m.plugins[idx].fileSize = fileSize
	} else {
		m.pluginMap[name] = len(m.plugins)
		m.plugins = append(m.plugins, pluginEntry{name: name, plugin: compiledPlugin, fileSize: fileSize})
	}
	m.mu.Unlock()

	m.logger.Info("Compiled",
		slog.String("name", name),
		slog.String("path", path),
		slog.Duration("duration", time.Since(start)),
	)

	return nil
}

// LoadAll loads multiple plugins concurrently using parallel compilation
// workers. paths is a map of plugin name -> file path.
func (m *Manager) LoadAll(ctx context.Context, paths map[string]string) error {
	if err := m.initCache(); err != nil {
		return fmt.Errorf("failed to initialize compilation cache: %v", err)
	}

	workers := m.numWorkers
	if workers <= 0 {
		workers = 1
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)

	for name, path := range paths {
		name, path := name, path
		g.Go(func() error {
			return m.Load(ctx, name, path)
		})
	}

	return g.Wait()
}

// IsLoaded returns whether the manager has any plugins loaded.
func (m *Manager) IsLoaded() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.plugins) > 0
}

// InstantiateAll creates instances of all loaded plugins concurrently while
// maintaining their original registration order.
func (m *Manager) InstantiateAll(ctx context.Context) (*Registry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	registry := newRegistry()
	registry.manager = m

	instances := make([]*extism.Plugin, len(m.plugins))

	g, ctx := errgroup.WithContext(ctx)

	for i, p := range m.plugins {
		i, p := i, p
		g.Go(func() error {
			pluginInstance, err := p.plugin.Instance(ctx, extism.PluginInstanceConfig{})
			if err != nil {
				return fmt.Errorf("failed to instantiate plugin '%s': %v", p.name, err)
			}
			pluginInstance.SetLogger(m.logAdapter(p.name))
			instances[i] = pluginInstance
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		for _, instance := range instances {
			if instance != nil {
				instance.Close(ctx)
			}
		}
		return nil, err
	}

	for i, p := range m.plugins {
		registry.plugins = append(registry.plugins, registryEntry{name: p.name, plugin: instances[i]})
		registry.pluginMap[p.name] = i
	}

	return registry, nil
}

// Close releases all resources associated with the manager.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for _, p := range m.plugins {
		if err := p.plugin.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.name, err))
		}
	}

	if m.sr != nil {
		if err := m.sr.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shared runtime: %w", err))
		}
	}

	if m.runtimeCache != nil {
		if err := m.runtimeCache.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("compilation cache: %w", err))
		}
	}

	return errors.Join(errs...)
}

// Metadata returns metadata for all loaded plugins.
func (m *Manager) Metadata() []Metadata {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]Metadata, len(m.plugins))
	for i, p := range m.plugins {
		result[i] = Metadata{
			Name:     p.name,
			FileSize: p.fileSize,
		}
	}
	return result
}

// Logger returns the manager's logger.
func (m *Manager) Logger() *slog.Logger {
	return m.logger
}

func (m *Manager) initCache() error {
	var initErr error
	m.cacheInit.Do(func() {
		if m.cachePath != "" {
			cache, err := wazero.NewCompilationCacheWithDir(m.cachePath)
			if err != nil {
				initErr = err
				return
			}
			m.runtimeCache = cache
			m.runtimeConfig = m.runtimeConfig.WithCompilationCache(cache)
		}

		// Initialize shared runtime if enabled. Uses background context because
		// sync.Once captures the closure — subsequent calls see the already-
		// initialized result regardless of caller context.
		if m.sharedRuntime {
			sr, err := extism.NewSharedRuntime(context.Background(), m.runtimeConfig, true)
			if err != nil {
				initErr = err
				return
			}
			m.sr = sr
		}
	})
	return initErr
}

func (m *Manager) logAdapter(name string) func(extism.LogLevel, string) {
	pluginLogger := m.logger.With("plugin", name)
	return func(level extism.LogLevel, msg string) {
		switch level {
		case extism.LogLevelDebug:
			pluginLogger.Debug(msg)
		case extism.LogLevelInfo:
			pluginLogger.Info(msg)
		case extism.LogLevelWarn:
			pluginLogger.Warn(msg)
		case extism.LogLevelError:
			pluginLogger.Error(msg)
		default:
			pluginLogger.Info(msg)
		}
	}
}
