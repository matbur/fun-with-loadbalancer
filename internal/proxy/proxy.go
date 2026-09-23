package proxy

import "fun-with-loadbalancer/internal/balancer"

type Server struct {
	pool *balancer.Pool
}