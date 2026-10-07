package overlay

import (
	"context"
	"errors"
	"strings"
)

// ErrUnknownKind is returned when no painter is registered for a kind's prefix.
var ErrUnknownKind = errors.New("overlay: no painter registered for kind")

// Router is a Painter that sends each kind to the painter registered for its
// prefix. One controller shared by several owners needs one painter, and each
// owner's kinds share a prefix ("setup:", "mint:").
type Router struct {
	// routes maps a kind prefix to its painter. Set once at composition time,
	// before the controller paints.
	routes map[string]Painter
}

// NewRouter returns a router for the given prefix-to-painter routes.
func NewRouter(routes map[string]Painter) *Router {
	return &Router{routes: routes}
}

// Show sends o to the painter for its prefix.
func (r *Router) Show(ctx context.Context, o Overlay) error {
	p, err := r.route(o.Kind)
	if err != nil {
		return err
	}
	return p.Show(ctx, o)
}

// Hide clears o through the painter for its prefix.
func (r *Router) Hide(ctx context.Context, o Overlay) error {
	p, err := r.route(o.Kind)
	if err != nil {
		return err
	}
	return p.Hide(ctx, o)
}

func (r *Router) route(k Kind) (Painter, error) {
	for prefix, p := range r.routes {
		if strings.HasPrefix(string(k), prefix) {
			return p, nil
		}
	}
	return nil, ErrUnknownKind
}
