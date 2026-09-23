package balancer

import "sync/atomic"

type Pool struct {
	backends atomic.Value // []*Backend
	next     atomic.Uint64
}