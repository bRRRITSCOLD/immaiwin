package llm

import (
	"fmt"
	"sync"
)

// Factory builds a Provider from a connection's config map.
// Plug point for v2.0 custom providers — third parties register their
// factory at startup, then the workflow Connection of matching type
// resolves to their provider.
type Factory func(config map[string]string) (Provider, error)

// Registry holds connection-type → factory mappings.
//
// In-tree providers (anthropic, openai, ollama) call Register at init().
// Out-of-tree plugins call Register from a registration entrypoint at
// process startup before any agent runs.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry creates an empty Registry. Most code uses the package-level
// Default registry; expose a struct so tests can build isolated registries.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register installs a factory for a connection type. Re-registering an
// existing type returns an error — explicit replacement should use Replace.
func (r *Registry) Register(connectionType string, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[connectionType]; exists {
		return fmt.Errorf("llm: factory for %q already registered", connectionType)
	}
	r.factories[connectionType] = f
	return nil
}

// Replace overwrites an existing registration. Used by tests + by plugins
// that intentionally shadow a built-in.
func (r *Registry) Replace(connectionType string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[connectionType] = f
}

// Build returns a new Provider for the given connection type using its
// config map. Returns an error if no factory is registered.
func (r *Registry) Build(connectionType string, config map[string]string) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[connectionType]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: no provider registered for connection type %q", connectionType)
	}
	return f(config)
}

// Has reports whether a factory is registered for a connection type.
func (r *Registry) Has(connectionType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[connectionType]
	return ok
}

// Default is the package-level registry. Most code should call the
// package-level Register / Build helpers, which delegate here.
var Default = NewRegistry()

// Register is the package-level shortcut for Default.Register.
func Register(connectionType string, f Factory) error {
	return Default.Register(connectionType, f)
}

// Build is the package-level shortcut for Default.Build.
func Build(connectionType string, config map[string]string) (Provider, error) {
	return Default.Build(connectionType, config)
}
