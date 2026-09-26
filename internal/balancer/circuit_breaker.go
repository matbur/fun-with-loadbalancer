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

// probeTimeout bounds how long a half-open probe may keep its admission. A
// probe normally gives it up by reporting, but an aborted response makes
// ReverseProxy panic out of ServeHTTP, so neither reportSuccess nor
// reportFailure runs and the next request would be turned away until restart.
// Must exceed the longest legitimate request, which the proxy caps at the
// server's WriteTimeout (30s).
const probeTimeout = 30 * time.Second

// circuitBreaker protects a backend against bursts of transport-level
// failures (dial errors, timeouts). After threshold consecutive failures the
// circuit opens and the backend stops receiving traffic; once the cooldown
// elapses it half-opens and lets a single request through as a probe — a
// successful probe closes the circuit, a failed one re-opens it with a fresh
// cooldown. Concurrent callers are turned away until that probe reports, so a
// recovering backend is never handed the backlog queued behind it.
//
// All fields are atomics: selection (Pool.Next) and request reporting (proxy
// ServeHTTP) happen concurrently, on different goroutines, without sharing a
// lock.
type circuitBreaker struct {
	state    atomic.Int32 // circuitState
	failures atomic.Int32
	openedAt atomic.Int64 // unix nanos, when the circuit was last opened

	// probeAt is when the half-open probe was admitted, or 0 when no probe is
	// in progress. Admission is held until that request reports, so one request
	// at a time finds out whether the backend is back.
	probeAt atomic.Int64

	threshold int32         // immutable after construction
	cooldown  time.Duration // immutable after construction
}

// allow reports whether the backend may currently take traffic. It also
// performs the lazy open → half-open transition once the cooldown elapsed,
// after which exactly one caller is admitted and the rest are turned away until
// that request reports or exceeds probeTimeout.
func (cb *circuitBreaker) allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitHalfOpen:
		return cb.claimProbe()
	case circuitOpen:
		now := time.Now().UnixNano()
		if now-cb.openedAt.Load() < int64(cb.cooldown) {
			return false
		}
		if !cb.state.CompareAndSwap(int32(circuitOpen), int32(circuitHalfOpen)) {
			return false
		}
		return cb.claimProbe()
	default:
		return true
	}
}

// claimProbe reports whether the caller is the one request allowed through
// while the circuit is half-open. The CAS admits a caller when no probe is in
// progress and takes over from one admitted longer ago than probeTimeout, so
// only one racing goroutine wins either way.
func (cb *circuitBreaker) claimProbe() bool {
	now := time.Now().UnixNano()
	held := cb.probeAt.Load()
	if held != 0 && now-held < int64(probeTimeout) {
		return false // the probe admitted this recently is still in progress
	}
	return cb.probeAt.CompareAndSwap(held, now)
}

// isOpen reports whether the circuit is currently open. Unlike allow it is a
// passive read: it never performs the open → half-open transition.
func (cb *circuitBreaker) isOpen() bool {
	return circuitState(cb.state.Load()) == circuitOpen
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
// circuit. Success while open is ignored — no request is admitted in that
// state, so it can only be a request admitted before the circuit tripped, and
// letting one success close it would defeat the breaker. See recover for the
// one signal allowed to override an open circuit.
func (cb *circuitBreaker) reportSuccess() {
	if circuitState(cb.state.Load()) == circuitHalfOpen {
		cb.close()
	}
	cb.failures.Store(0)
}

// recover closes an open or half-open circuit on an out-of-band signal that the
// backend is reachable (today: the health checker's /healthz probe). It is the
// only way back for an idle load balancer, where no request ever wins the
// open → half-open transition. It is a no-op in closed state on purpose: the
// failure counter measures request handling, and clearing it on every health
// check would stop a backend failing one request in five from reaching
// threshold.
func (cb *circuitBreaker) recover() {
	switch circuitState(cb.state.Load()) {
	case circuitOpen, circuitHalfOpen:
		cb.close()
		cb.failures.Store(0)
	}
}

// close returns the circuit to closed state and ends any half-open probe.
func (cb *circuitBreaker) close() {
	cb.probeAt.Store(0)
	cb.state.Store(int32(circuitClosed))
}

// open switches the circuit to open with a fresh cooldown, resets the
// consecutive-failure counter and ends any half-open probe.
func (cb *circuitBreaker) open() {
	cb.openedAt.Store(time.Now().UnixNano())
	cb.failures.Store(0)
	cb.probeAt.Store(0)
	cb.state.Store(int32(circuitOpen))
}