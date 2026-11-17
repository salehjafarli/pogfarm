package source

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// ErrUnknownPlatform is returned (wrapped) by Registry.Get when no source is
// registered for the platform.
var ErrUnknownPlatform = errors.New("source: unknown platform")

// Registry maps a platform name to the Source that serves it. The zero value
// is an empty registry ready to use. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	sources map[api.Platform]Source
}

// NewRegistry returns a registry holding the given sources. It fails where
// Register would.
func NewRegistry(sources ...Source) (*Registry, error) {
	r := &Registry{}
	for _, s := range sources {
		if err := r.Register(s); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds s under the name s.Platform() returns. It fails if s is nil,
// the name is empty, or a source is already registered under that name.
func (r *Registry) Register(s Source) error {
	if s == nil {
		return errors.New("source: register nil source")
	}
	p := s.Platform()
	if p == "" {
		return errors.New("source: register source with empty platform name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[p]; ok {
		return fmt.Errorf("source: platform %q is already registered", p)
	}
	if r.sources == nil {
		r.sources = make(map[api.Platform]Source)
	}
	r.sources[p] = s
	return nil
}

// Get returns the source registered for the platform, or an error wrapping
// ErrUnknownPlatform.
func (r *Registry) Get(p api.Platform) (Source, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sources[p]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPlatform, p)
	}
	return s, nil
}

// Platforms returns the registered platform names in sorted order.
func (r *Registry) Platforms() []api.Platform {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]api.Platform, 0, len(r.sources))
	for p := range r.sources {
		names = append(names, p)
	}
	slices.Sort(names)
	return names
}
