package wasm

import (
	"context"
	"fmt"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
)

// Pool maintains a fixed-size pool of pre-instantiated plugin registries,
// one per PHP thread. Each slot is thread-private — no mutex needed on the
// hot path. Plugins are lazily instantiated on first call within a request.
type Pool struct {
	manager *Manager
	size    int

	// slots[i] is the registry for PHP thread i. When not checked out,
	// the registry sits here ready for immediate use.
	slots chan *Registry

	// compiled plugins, used for lazy instantiation inside registries
	compiled []pluginEntry
}

// NewPool creates a pool of pre-instantiated registries.
// size should match the number of PHP threads for zero-contention dispatch.
func NewPool(ctx context.Context, manager *Manager, size int) (*Pool, error) {
	// Validate pool size
	if size <= 0 {
		return nil, fmt.Errorf("pool size must be positive, got %d", size)
	}

	manager.mu.RLock()
	compiled := make([]pluginEntry, len(manager.plugins))
	copy(compiled, manager.plugins)
	manager.mu.RUnlock()

	pool := &Pool{
		manager:  manager,
		size:     size,
		slots:    make(chan *Registry, size),
		compiled: compiled,
	}

	// Pre-create empty registries — plugins are lazily instantiated on first call
	for i := 0; i < size; i++ {
		reg := &Registry{
			pluginMap:     make(map[string]int),
			instances:     make(map[string]*extism.Plugin),
			manager:       manager,
			compiled:      compiled,
			lazyInstances: true,
		}
		// Pre-populate the name→index map so Exists/Names work without instantiation
		for j, p := range compiled {
			reg.pluginMap[p.name] = j
			reg.plugins = append(reg.plugins, registryEntry{name: p.name})
		}
		pool.slots <- reg
	}

	return pool, nil
}

// Get retrieves a registry from the pool, blocking if none are available.
// The returned registry is exclusively owned by the caller until Put is called.
func (p *Pool) Get(ctx context.Context) (*Registry, error) {
	select {
	case reg, ok := <-p.slots:
		if !ok {
			return nil, ErrPoolClosed
		}
		return reg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Put returns a registry to the pool after resetting its state.
// Plugins remain instantiated for reuse on the next request.
// Returns a sentinel error if the channel is closed.
func (p *Pool) Put(reg *Registry) error {
	reg.Reset()
	select {
	case p.slots <- reg:
		return nil
	default:
		return ErrPoolClosed
	}
}

// Close drains the pool and closes all registries.
func (p *Pool) Close(ctx context.Context) error {
	close(p.slots)
	var errs []error
	for reg := range p.slots {
		if err := reg.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pool close errors: %v", errs)
	}
	return nil
}

// WarmUp pre-instantiates all plugins in all pool slots concurrently.
// Call this at startup after NewPool to pay the instantiation cost upfront.
// Slots that fail instantiation are still returned to the pool — they'll
// fall back to lazy instantiation on demand.
func (p *Pool) WarmUp(ctx context.Context) error {
	// Drain all slots with a timeout (pool should be idle at startup)
	registries := make([]*Registry, 0, p.size)
	timeout := time.After(30 * time.Second)
	for i := 0; i < p.size; i++ {
		select {
		case reg, ok := <-p.slots:
			if !ok {
				// Put back what we have and bail
				for _, r := range registries {
					p.slots <- r
				}
				return ErrPoolClosed
			}
			registries = append(registries, reg)
		case <-timeout:
			// Put back what we have and bail
			for _, r := range registries {
				p.slots <- r
			}
			return fmt.Errorf("warmup timeout waiting for pool slot %d/%d", i, p.size)
		case <-ctx.Done():
			for _, r := range registries {
				p.slots <- r
			}
			return ctx.Err()
		}
	}

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup

	for _, reg := range registries {
		wg.Add(1)
		go func(r *Registry) {
			defer wg.Done()
			if err := r.InstantiateAll(ctx); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(reg)
	}
	wg.Wait()

	// Return all slots — failed ones still have lazyInstances=true
	// and will instantiate on demand
	for _, reg := range registries {
		p.slots <- reg
	}

	if len(errs) > 0 {
		return fmt.Errorf("warmup errors: %v", errs)
	}
	return nil
}
