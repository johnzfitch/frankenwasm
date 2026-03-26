package wasm

import (
	"context"
	"errors"
	"fmt"

	extism "github.com/extism/go-sdk"
)

var (
	ErrPluginNotFound   = errors.New("plugin not found")
	ErrFunctionNotFound = errors.New("function not found")
)

type registryEntry struct {
	name   string
	plugin *extism.Plugin
}

// Registry maintains an ordered collection of plugin instances.
// When used via Pool, each registry is request-private — no locks needed.
// Plugins can be eagerly or lazily instantiated.
type Registry struct {
	plugins   []registryEntry
	pluginMap map[string]int // name -> index
	manager   *Manager

	// Lazy instantiation support
	compiled      []pluginEntry
	instances     map[string]*extism.Plugin // lazily populated
	lazyInstances bool
}

func newRegistry() *Registry {
	return &Registry{
		plugins:   make([]registryEntry, 0),
		pluginMap: make(map[string]int),
	}
}

// Call invokes a plugin's function with the provided arguments.
// No mutex — when used via Pool, the registry is request-private.
// Uses the fast-path Extism call that skips error checking on success
// and avoids the defensive output copy.
func (r *Registry) Call(ctx context.Context, name, function string, args []byte) ([]byte, error) {
	plugin, err := r.getOrInstantiate(ctx, name)
	if err != nil {
		return nil, err
	}

	if !plugin.FunctionExists(function) {
		return nil, fmt.Errorf("%w: %s", ErrFunctionNotFound, function)
	}

	// FastCall skips error_get on rc==0, skips output copy
	output, err := plugin.FastCall(ctx, function, args)
	if err != nil {
		return nil, err
	}

	return output, nil
}

// getOrInstantiate returns the plugin instance, instantiating lazily if needed.
func (r *Registry) getOrInstantiate(ctx context.Context, name string) (*extism.Plugin, error) {
	// Fast path: already instantiated
	if r.lazyInstances {
		if inst, ok := r.instances[name]; ok {
			return inst, nil
		}
	}

	idx, ok := r.pluginMap[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPluginNotFound, name)
	}

	// If not lazy, just return from the entry
	if !r.lazyInstances {
		return r.plugins[idx].plugin, nil
	}

	// Lazy instantiation
	if idx >= len(r.compiled) {
		return nil, fmt.Errorf("%w: %s (index out of range)", ErrPluginNotFound, name)
	}

	p := r.compiled[idx]
	inst, err := p.plugin.Instance(ctx, extism.PluginInstanceConfig{})
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate plugin '%s': %w", name, err)
	}
	inst.SetLogger(r.manager.logAdapter(name))
	r.instances[name] = inst
	r.plugins[idx].plugin = inst

	return inst, nil
}

// Get returns a plugin with the given name from the registry.
func (r *Registry) Get(name string) *extism.Plugin {
	idx, ok := r.pluginMap[name]
	if !ok {
		return nil
	}
	// For lazy registries, check instances map first
	if r.lazyInstances {
		if inst, ok := r.instances[name]; ok {
			return inst
		}
		return nil
	}
	return r.plugins[idx].plugin
}

// Exists checks if a plugin with the given name exists in the registry.
func (r *Registry) Exists(name string) bool {
	_, ok := r.pluginMap[name]
	return ok
}

// Names returns a slice of all plugin names in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.plugins))
	for i, p := range r.plugins {
		names[i] = p.name
	}
	return names
}

// Metadata returns metadata for all loaded plugins.
func (r *Registry) Metadata() []Metadata {
	if r.manager != nil {
		return r.manager.Metadata()
	}
	return nil
}

// Len returns the number of plugins in the registry.
func (r *Registry) Len() int {
	return len(r.plugins)
}

// Clone creates a new registry by re-instantiating all plugins through the manager.
// Kept for backward compatibility — prefer Pool for production use.
func (r *Registry) Clone(ctx context.Context) (*Registry, error) {
	if r.manager == nil {
		return newRegistry(), nil
	}

	registry, err := r.manager.InstantiateAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to clone registry: %w", err)
	}

	return registry, nil
}

// InstantiateAll eagerly instantiates all plugins in this registry.
// Used by Pool.WarmUp to pay instantiation cost upfront.
func (r *Registry) InstantiateAll(ctx context.Context) error {
	if !r.lazyInstances {
		return nil
	}

	for _, p := range r.compiled {
		if _, ok := r.instances[p.name]; ok {
			continue
		}

		inst, err := p.plugin.Instance(ctx, extism.PluginInstanceConfig{})
		if err != nil {
			return fmt.Errorf("failed to instantiate plugin '%s': %w", p.name, err)
		}
		inst.SetLogger(r.manager.logAdapter(p.name))
		r.instances[p.name] = inst

		if idx, ok := r.pluginMap[p.name]; ok {
			r.plugins[idx].plugin = inst
		}
	}

	return nil
}

// Reset prepares the registry for reuse by the next request.
// Plugin instances remain alive — only per-request state is cleared.
func (r *Registry) Reset() {
	// Currently plugin instances are stateless between calls (Extism resets
	// internal state on each Call). No per-request state to clear beyond
	// what Extism already handles. This method exists as the extension point
	// for future per-request cleanup.
}

// Close releases all plugins and their associated resources.
func (r *Registry) Close(ctx context.Context) error {
	var errs []error
	if r.lazyInstances {
		for name, inst := range r.instances {
			if err := inst.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("plugin %s: %w", name, err))
			}
		}
	} else {
		for _, p := range r.plugins {
			if p.plugin != nil {
				if err := p.plugin.Close(ctx); err != nil {
					errs = append(errs, fmt.Errorf("plugin %s: %w", p.name, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}
