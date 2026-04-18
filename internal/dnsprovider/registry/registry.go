// Package registry is the factory registry for DNS providers. Each provider
// self-registers from its init() so cmd/main only needs a blank-import;
// provider selection then happens at runtime via the "provider" config key.
package registry

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/math280h/greydns/internal/dnsprovider"
)

// Factory constructs a concrete provider from its slice of configuration and
// the greydns secret data. Providers read their namespaced keys via
// ProviderConfig helpers.
type Factory func(cfg ProviderConfig, secret map[string][]byte) (dnsprovider.Provider, error)

// ProviderConfig exposes typed accessors over the full ConfigMap data.
// Generic (un-namespaced) keys are read by the controller, not providers.
type ProviderConfig struct {
	Provider string
	Data     map[string]string
}

func NewProviderConfig(provider string, data map[string]string) ProviderConfig {
	return ProviderConfig{Provider: provider, Data: data}
}

func (c ProviderConfig) key(suffix string) string {
	return c.Provider + "." + suffix
}

func (c ProviderConfig) GetRequired(suffix string) (string, error) {
	v, ok := c.Data[c.key(suffix)]
	if !ok {
		return "", fmt.Errorf("missing required config key %q", c.key(suffix))
	}
	return v, nil
}

func (c ProviderConfig) GetOptional(suffix, defaultValue string) string {
	if v, ok := c.Data[c.key(suffix)]; ok {
		return v
	}
	return defaultValue
}

func (c ProviderConfig) GetBool(suffix string, defaultValue bool) bool {
	raw, ok := c.Data[c.key(suffix)]
	if !ok {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return defaultValue
	}
	return parsed
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{} //nolint:gochecknoglobals // registry pattern
)

// Register adds a provider factory under name. Intended for use from an
// init() in each provider package. Panics on duplicate registration, which
// is a programming error.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("dnsprovider: duplicate registration for %q", name))
	}
	factories[name] = f
}

func Build(name string, cfg ProviderConfig, secret map[string][]byte) (dnsprovider.Provider, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("dnsprovider: no provider registered as %q (did you blank-import it?)", name)
	}
	return f(cfg, secret)
}

func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	return names
}
