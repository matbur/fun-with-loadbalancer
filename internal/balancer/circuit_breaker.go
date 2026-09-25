package balancer

import (
	"sync/atomic"
	"time"
)

// circuitState is the state of a backend's circuit breaker.
type circuitState int32

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// Default circuit breaker parameters, applied by NewBackend unless
// overridden with WithBreakerThreshold/WithBreakerCooldown.
const (
	defaultBreakerThreshold = 3
	defaultBreakerCooldown  = 5 * time.Second
)

// circuitBreaker protects a backend against bursts of transport-level
// failures (dial errors, timeouts). After threshold consecutive failures the
// circuit opens and the backend stops receiving traffic; once the cooldown
// elapses it half-opens and lets requests through as probes — a successful
// probe closes the circuit, a failed one re-opens it with a fresh cooldown.
//
// All fields are atomics: selection (Pool.Next) and request reporting (proxy
// ServeHTTP) happen concurrently, on different goroutines, without sharing a
// lock.
type circuitBreaker struct {
	state    atomic.Int32 // circuitState
	failures atomic.Int32
	openedAt atomic.Int64 // unix nanos, when the circuit was last opened

	threshold int32        // immutable after construction
	cooldown  time.Duration // immutable after construction
}

// allow reports whether the backend may currently take traffic. It also
// performs the lazy open → half-open transition once the cooldown elapsed;
// the goroutine that wins the CAS becomes the first probe, the losers are
// turned away for this call only.
func (cb *circuitBreaker) allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitHalfOpen:
		return true
	case circuitOpen:
		now := time.Now().UnixNano()
		if now-cb.openedAt.Load() < int64(cb.cooldown) {
			return false
		}
		if cb.state.CompareAndSwap(int32(circuitOpen), int32(circuitHalfOpen)) {
			return true
		}
		return false
	default:
		return true
	}
}

// reportFailure feeds a transport error into the breaker. In closed state it
// counts consecutive failures; a half-open probe failure re-opens the circuit
// immediately. Failures while open are ignored (the circuit is already open).
func (cb *circuitBreaker) reportFailure() {
	switch circuitState(cb.state.Load()) {
	case circuitHalfOpen:
		cb.open()
	case circuitClosed:
		if cb.failures.Add(1) >= cb.threshold {
			cb.open()
		}
	}
}

// reportSuccess feeds a successful request (no transport error) into the
// breaker: it resets the failure counter and, in half-open state, closes the
// circuit. Success while open is ignored — probes are only allowed once the
// cooldown elapsed.
func (cb *circuitBreaker) reportSuccess() {
	if circuitState(cb.state.Load()) == circuitHalfOpen {
		cb.state.Store(int32(circuitClosed))
	}
	cb.failures.Store(0)
}

// open switches the circuit to open with a fresh cooldown and resets the
// consecutive-failure counter.
func (cb *circuitBreaker) open() {
	cb.openedAt.Store(time.Now().UnixNano())
	cb.failures.Store(0)
	cb.state.Store(int32(circuitOpen))
}