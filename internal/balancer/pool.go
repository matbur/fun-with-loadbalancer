package balancer

import (
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

type Pool struct {
	backends atomic.Value // []*Backend
	next     atomic.Uint64

	alg Algorithm

	// State for smooth weighted round-robin, guarded by mu.
	mu              sync.Mutex
	weightedCurrent []int
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
		if candidate.Healthy.Load() {
			return candidate
		}
	}

	return nil
}

// nextWeighted implements smooth weighted round-robin (nginx algorithm):
// every call adds each backend's weight to its current value, then the
// backend with the highest current value is selected and its current value is
// reduced by the total weight. Only healthy backends participate, so when one
// is down the others share its load proportionally.
func (p *Pool) nextWeighted() *Backend {
	backends := p.backends.Load().([]*Backend)
	if len(backends) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.weightedCurrent) != len(backends) {
		p.weightedCurrent = make([]int, len(backends))
	}

	total := 0
	best := -1
	for i := range backends {
		if !backends[i].Healthy.Load() {
			p.weightedCurrent[i] = 0
			continue
		}
		w := backends[i].Weight()
		p.weightedCurrent[i] += w
		total += w
		if best == -1 || p.weightedCurrent[i] > p.weightedCurrent[best] {
			best = i
		}
	}
	if best == -1 {
		return nil
	}

	p.weightedCurrent[best] -= total
	return backends[best]
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
		if !b.Healthy.Load() {
			continue
		}
		if best == nil || b.Active() < best.Active() {
			best = b
		}
	}
	return best
}