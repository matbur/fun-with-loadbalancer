package balancer

import "sync/atomic"

type Pool struct {
	backends atomic.Value // []*Backend
	next     atomic.Uint64
}

func NewPool(backends []*Backend) *Pool {
	p := &Pool{}
	p.backends.Store(backends)
	return p
}

func (p *Pool) Next() *Backend {
	backends := p.backends.Load().([]*Backend)
	if len(backends) == 0 {
		return nil
	}

	start := p.next.Add(1)

	for i := uint64(0); i < uint64(len(backends)); i++ {
		candidate := backends[(start+i)%uint64(len(backends))]
		if candidate.Healthy.Load() {
			return candidate
		}
	}

	return nil
}
