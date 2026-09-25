package balancer

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
)

// Algorithm selects how Pool.Next() picks a backend.
type Algorithm int

const (
	// AlgorithmRoundRobin is the default: an atomic counter advances over the
	// healthy backends in order.
	AlgorithmRoundRobin Algorithm = iota
	// AlgorithmWeightedRoundRobin is smooth weighted round-robin (nginx):
	// a backend with weight w is selected ~w times more often than a backend
	// with weight 1. Unhealthy backends are skipped and their share is
	// redistributed among the healthy ones.
	AlgorithmWeightedRoundRobin
	// AlgorithmLeastConnections picks the healthy backend with the fewest
	// in-flight requests (see Backend.Acquire/Release).
	AlgorithmLeastConnections
)

var (
	// ErrDuplicateBackend is returned by (Pool).AddBackend when the backend is
	// already part of the pool.
	ErrDuplicateBackend = errors.New("backend already in pool")

	// ErrBackendNotFound is returned by (Pool).RemoveBackend when the backend
	// is not part of the pool.
	ErrBackendNotFound = errors.New("backend not in pool")
)

type Pool struct {
	backends atomic.Value // []*Backend
	next     atomic.Uint64

	alg Algorithm

	// mu serializes smooth weighted round-robin selection (Backend.currentWeight)
	// and list mutations (AddBackend/RemoveBackend). Next() for the other
	// algorithms reads the atomic backends snapshot without taking it.
	mu sync.Mutex
}

type PoolOption func(*Pool)

// WithAlgorithm overrides the default round-robin selection algorithm.
func WithAlgorithm(a Algorithm) PoolOption {
	return func(p *Pool) {
		p.alg = a
	}
}

func NewPool(backends []*Backend, opts ...PoolOption) *Pool {
	p := &Pool{}
	p.backends.Store(backends)
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Pool) Backends() []*Backend {
	return append([]*Backend(nil), p.backends.Load().([]*Backend)...)
}

// AddBackend adds b to the pool. It returns ErrDuplicateBackend if b is
// already present. The new list is stored as a single atomic snapshot, so
// concurrent Next() calls either see b or do not — never a partial list.
func (p *Pool) AddBackend(b *Backend) error {
	if b == nil {
		return errors.New("balancer: nil backend")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	backends := p.backends.Load().([]*Backend)
	if slices.Contains(backends, b) {
		return ErrDuplicateBackend
	}

	next := make([]*Backend, 0, len(backends)+1)
	next = append(next, backends...)
	next = append(next, b)
	p.backends.Store(next)
	return nil
}

// RemoveBackend removes b from the pool and resets its smooth weighted
// round-robin state, so a re-added backend starts with a clean accumulator.
// It returns ErrBackendNotFound if b is not in the pool. Requests already
// in flight to b finish normally — Next() simply stops returning it.
func (p *Pool) RemoveBackend(b *Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	backends := p.backends.Load().([]*Backend)
	for i, existing := range backends {
		if existing == b {
			next := make([]*Backend, 0, len(backends)-1)
			next = append(next, backends[:i]...)
			next = append(next, backends[i+1:]...)
			p.backends.Store(next)

			b.currentWeight = 0
			return nil
		}
	}
	return ErrBackendNotFound
}

func (p *Pool) Next() *Backend {
	switch p.alg {
	case AlgorithmWeightedRoundRobin:
		return p.nextWeighted()
	case AlgorithmLeastConnections:
		return p.nextLeastConnections()
	default:
		return p.nextRoundRobin()
	}
}

func (p *Pool) nextRoundRobin() *Backend {
	backends := p.backends.Load().([]*Backend)
	if len(backends) == 0 {
		return nil
	}

	start := p.next.Add(1)

	for i := range len(backends) {
		candidate := backends[(start+uint64(i))%uint64(len(backends))]
		if candidate.Available() {
			return candidate
		}
	}

	return nil
}

// nextWeighted implements smooth weighted round-robin (nginx algorithm):
// every call adds each backend's weight to its currentWeight, then the
// backend with the highest currentWeight is selected and its currentWeight is
// reduced by the total weight. Only healthy backends participate, so when one
// is down the others share its load proportionally. The accumulator lives on
// each Backend (Backend.currentWeight), which keeps the state correct when
// backends are added or removed dynamically.
func (p *Pool) nextWeighted() *Backend {
	backends := p.backends.Load().([]*Backend)
	if len(backends) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	total := 0
	var best *Backend
	for _, b := range backends {
		if !b.Available() {
			b.currentWeight = 0
			continue
		}
		w := b.Weight()
		b.currentWeight += w
		total += w
		if best == nil || b.currentWeight > best.currentWeight {
			best = b
		}
	}
	if best == nil {
		return nil
	}

	best.currentWeight -= total
	return best
}

// nextLeastConnections picks the healthy backend with the lowest number of
// in-flight requests (Backend.Active). Ties are broken by the first backend
// in the list.
func (p *Pool) nextLeastConnections() *Backend {
	backends := p.backends.Load().([]*Backend)
	if len(backends) == 0 {
		return nil
	}

	var best *Backend
	for _, b := range backends {
		if !b.Available() {
			continue
		}
		if best == nil || b.Active() < best.Active() {
			best = b
		}
	}
	return best
}
