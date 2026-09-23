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
	for _, b := range backends {
		if b.Healthy.Load() {
			return b
		}
	}
	return nil
}