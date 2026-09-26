# Code review — `fun-with-loadbalancer`

Scope: whole project (all non-test and test Go sources, `README.md`, `examples/lb.yaml`).
Baseline: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...` all pass.
Findings below were reproduced with throwaway tests (since removed).

Status: **1, 2, 3 — fixed. 4 — partially fixed** (see the resolution note under each finding). 5–8 open.

---

## 1. [Medium] ✅ FIXED — In-flight counter leaks whenever a response copy aborts (GET/HEAD/OPTIONS only)

`internal/proxy/proxy.go:196-199`

```go
backend.Acquire()
tw.reset()
backend.Proxy.ServeHTTP(tw, r)
backend.Release()   // not deferred
```

`httputil.ReverseProxy` panics with `http.ErrAbortHandler` when copying the response body fails
mid-stream — i.e. the client hangs up early, or the upstream resets the connection. The
`http.Server` recovers that panic, so the request unwinds through `serveWithRetry` and
`Release()` is never reached. `serveDirect` (`proxy.go:151-152`) pairs `Acquire`/`Release` with
`defer` and is unaffected, so the two code paths differ exactly on the retryable methods.

Measured, through a real `httptest` server, with a backend that writes a partial response and
then closes the connection:

| aborted requests | `Active()` after |
|---|---|
| 3 × `GET`  (→ `serveWithRetry`)  | **3** |
| 3 × `POST` (→ `serveDirect`)     | 0 |

Consequences, both permanent for the process lifetime:

- `-algorithm least-connections`: `nextLeastConnections` (`pool.go:212`) compares `Active()`, so
  the backend looks permanently busier than it is and is progressively starved — it is only picked
  once every other backend has at least as many in-flight requests. There is no decay, so the skew
  accumulates over the whole run.
- `GET /backends` reports a bogus `active` value.
- Secondary: the panic also skips `logAccess`, so aborted requests leave **no access log line** —
  invisible in the access log and in `lb_http_requests_total` (that one still counts, via the
  metrics wrapper).

The scenario is not exotic: any client that closes the tab during a slow response, or an upstream
that resets mid-body, leaks one increment per occurrence.

Suggested fix: make the pairing panic-safe, e.g. run each attempt through a helper that defers the
release, matching `serveDirect`. While there, consider whether an aborted response deserves its own
log line, and whether `RemoveBackend` should reset `active` alongside `currentWeight`
(`pool.go:124`) so a re-added backend starts clean.

### Resolution

Done in `internal/proxy/proxy.go`: the "route one request to a backend" step is now the single
helper `serveOn` (`proxy.go:218-232`), which defers the release, and both `serveDirect` and
`serveWithRetry` call it. The release now runs during panic unwinding, so an aborted response
gives back its in-flight count. No change on the normal path (`tw.reset()` on the fresh writer in
`serveDirect` is a no-op; the metric observation still happens before `Acquire`).

Regression test: `TestServeHTTPReleasesInFlightSlotOnAbortedResponse` in
`internal/proxy/proxy_test.go`, driving a raw-TCP backend that promises `Content-Length: 1024`,
sends 7 bytes and drops the connection. It must run behind a real `httptest.NewServer` — the
stdlib only raises the `ErrAbortHandler` panic when `http.ServerContextKey` is in the request
context, so with `httptest.NewRecorder` the abort would never panic and the test would pass
vacuously. It also counts backend hits so a request that never reached the backend cannot look
like a pass. Verified failing before the fix (`Active()` = 2 leaked) and passing after.

Verified: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`,
`go test -count=30 -run TestServeHTTP ./internal/proxy/`.

Still open from this finding, deliberately:

- Aborted requests produce no access-log line (the panic skips `logAccess` too). Fixing it means
  recover-and-repanic around the attempt, and `http.ErrAbortHandler` is routine control flow in
  `net/http` (a client closing the tab), so a `warn` line per occurrence would be noise.
- `RemoveBackend` still does not reset `active`; the leak is gone, so this is only cosmetic.

---

## 2. [Low-Medium] ✅ FIXED — The catch-all `/` proxy handler defeats the method-scoped admin/probe patterns — and two tests assert a `405` that never happens in production

`cmd/lb/main.go:100-109` registers `mux.Handle("/", …)` (the proxy) alongside the method-scoped
`GET /backends`, `POST /backends`, `DELETE /backends`, `GET /live`, `GET /ready`. In
`net/http.ServeMux` a method-mismatched request against a method-scoped pattern is not a `405` if
some other pattern also matches the path, so `/` wins and the request is proxied to a backend.

Reproduced against the exact mux from `main()`:

```
POST   /live            -> 200  proxied to backend
DELETE /ready           -> 200  proxied to backend
PUT    /backends        -> 200  proxied to backend
```

So the method restrictions on the admin endpoints are not actually enforced in the shipped
binary: a `PUT /backends` (a plausible operator typo) is silently forwarded to an application
backend instead of being rejected.

Worse, two tests pin the opposite behavior against a *different* mux that has no catch-all, so
they pass and hide the gap:

- `cmd/lb/admin_test.go:143` `TestAdminWrongMethodNotAllowed`
- `cmd/lb/healthz_test.go:73` `TestHealthzWrongMethodNotAllowed`

Suggested fix: build the mux in one function used by both `main()` and the tests (then the 405
assertions become meaningful), and/or have the `/` handler refuse the reserved prefixes
(`/metrics`, `/backends`, `/live`, `/ready`) instead of shadowing them.

### Resolution

Done. The mux is now built by `newMux(pool, admin, m, proxied)` in the new `cmd/lb/routes.go`, used
by `main()` and by the tests, so there is one routing definition.

The reserved paths keep their own paths in the mux but drop the method-scoped pattern in favour of an
explicit `methodRouter` (`routes.go:19-30`) — a `map[string]http.HandlerFunc` that dispatches on
`r.Method` and answers `405` with a sorted `Allow` header otherwise. Registering `/backends`,
`/live`, `/ready` per path (all methods) makes them more specific than `/`, so the catch-all can no
longer win; the proxy stays the catch-all for everything else.

Two details preserved deliberately: `HEAD` is registered next to `GET` on each path, because the
method-scoped `"GET"` patterns it replaces also matched `HEAD`; and `/metrics` is unchanged, since it
was already registered per path and so was never affected by this finding.

Tests: `cmd/lb/routes_test.go` adds `TestReservedPathsRejectWrongMethod` (405 + `Allow` for every
method a reserved path does not implement), `TestReservedPathsServeGet` (the happy paths still work),
`TestProxyStillCatchAll` (including `/backends/extra` and `/live/deep`, which are not reserved and
must still proxy) and `TestMethodRouter`. The two pre-existing 405 tests now build the production mux
and additionally assert the `Allow` header; their shared `notProxied(t)` stand-in fails the test if
the catch-all is ever reached, so a regression reports "was proxied to a backend" rather than a bare
status mismatch.

Verified: reverting `newMux` to the old method-scoped registration makes the new and updated tests
fail with `418` (proxy reached) instead of `405`. `go build ./...`, `go vet ./...`, `go test ./...`,
`go test -race ./...`, `go test -count=20 ./cmd/lb/` all pass.

One thing the new test caught while being written: `DELETE /backends` and `POST /backends` *are*
implemented methods, so they legitimately reach `admin.remove`/`admin.add` (and 400 on an empty body)
— only `PUT`/`PATCH` are wrong-method cases for that path.

Still open, deliberately: `/backends` remains unauthenticated, so a wrong method on a *correct* path
(`POST /backends` with junk) is still reachable by anyone who can reach the port. That is the
documented local/dev tradeoff, not part of this finding.

---

## 3. [Low-Medium] ✅ FIXED — Half-open lets every concurrent request through, not a single probe

`internal/balancer/circuit_breaker.go:44-62`

The doc comment states:

> the goroutine that wins the CAS becomes the first probe, the losers are turned away for this call only

In practice only callers that observe `open` *and* lose the CAS are turned away; everyone arriving
after the winner sees `circuitHalfOpen` and gets `return true`. Measured: after the cooldown,
**100 of 100** consecutive `allow()` calls were allowed through.

Impact: when a backend recovers, the entire concurrent backlog is dumped at it at once instead of
one probe. If the backend is still broken, all of those requests fail and each one re-opens the
circuit. The same intent is stated in `README.md:87` ("pierwszy request decyduje"). This is a
common simplification for small breakers, but the comment describes behavior the code does not
have. Either gate half-open with a concurrent-probe counter (allow N, then reject until a probe
resolves) or correct the comment to match reality.

### Resolution

Done, via the concurrent-probe option rather than by weakening the comment: half-open now admits
exactly one probe at a time.

`internal/balancer/circuit_breaker.go` gains a `probeAt atomic.Int64` — the unix-nano moment the
probe was admitted, or 0 when no probe is in progress. `claimProbe()` takes that admission with a
`CompareAndSwap`, so of the goroutines racing for it exactly one wins; `close()` and `open()` both
end it, so a verdict always gives it up. Measured before the fix: 100 of 100 sequential
`allow()` calls were admitted, and 64 of 64 under real goroutine contention; both are 1 after.

`probeTimeout = 30s` (`circuit_breaker.go:38`) bounds how long a probe may keep its admission, and
this bound is load-bearing, not defensive padding. The one case that never reports a verdict is the
abort path from finding 1: a response that fails mid-copy makes `httputil.ReverseProxy` panic out
of `ServeHTTP`, so neither `ReportSuccess` nor `ReportFailure` runs. With a plain counter that
probe would hold its admission forever, and since the health checker only writes `Backend.Healthy`
and nothing else ever moves the breaker, the backend would stay out of rotation until the process
restarts — turning this fix into a strictly worse outage than the one it repairs. A probe admitted
longer ago than `probeTimeout` is taken over by the next `claimProbe`. 30s matches the server's
`WriteTimeout`, so a legitimate slow probe is never cut off.

Tests added to `internal/balancer/circuit_breaker_test.go`:
`TestCircuitBreakerHalfOpenAdmitsSingleProbe` (the 100-call check),
`TestCircuitBreakerHalfOpenAdmitsSingleProbeUnderConcurrency` (64 goroutines released from a
`WaitGroup`, since that is the case a sequential check cannot reach),
`TestCircuitBreakerHalfOpenProbeEndsAdmission`,
`TestCircuitBreakerHalfOpenReclaimsAbandonedProbe` (backdates `probeAt` to simulate the abort
path — no 30s sleep) and `TestPoolHalfOpenBackendReceivesOneRequest` (pool-level: one request, then
`nil`, then back in rotation after the probe succeeds). Verified all four of the behavioural ones
fail against the old `allow()` and pass after. `go build ./...`, `go vet ./...`, `go test ./...`,
`go test -race ./...` and `go test -race -count=30 -run 'CircuitBreaker|HalfOpen'` all pass.

`README.md:87` updated to state the single-probe rule and the 30s reclaim, so the docs match the
code rather than the other way round.

One honest consequence, documented on `Backend.Available()`: the claim is a side effect, and
`nextWeighted`/`nextLeastConnections` rank candidates by calling `Available()` on each one, so a
half-open backend that *loses* the ranking has consumed an admission and stays out of rotation
until `probeTimeout` elapses. Fixing it properly means splitting passive availability from the claim
and re-ranking on a lost claim, which touches the two hot-path selection loops; the wart is
bounded, cannot misroute or produce spurious 503s, and is strictly an improvement on the previous
behaviour. The default round-robin scan returns the first candidate it admits and is unaffected.
Finding 4 narrows the window further: the health checker now closes a stuck circuit on its own.

Still open, deliberately: findings 4 and 6 both concern the same passive-vs-active asymmetry from
the other side (`/ready` reads `isOpen()`, the proxy reads `allow()`), and are not addressed here.

---

## 4. [Low] ⚠️ PARTIALLY FIXED — `/ready` can report `503` at moments when the proxy would still route traffic

`Pool.Ready()` (`pool.go:76-83`) uses `breaker.isOpen()`, a passive read, while the proxy path
uses `Available()` → `allow()`, which performs the lazy open → half-open transition once the
cooldown has elapsed. So a circuit that is open but *past its cooldown* is reported as
unavailable by `/ready` while `Next()` would hand out that backend. Reproduced:

```
just tripped:                       Ready=false isOpen=true  Next=nil
cooldown elapsed, still no traffic: isOpen=true  Ready=false  Next=<backend>
```

The passive read is deliberate and pinned by `TestReadyDoesNotTripBreaker`
(`cmd/lb/healthz_test.go:85`), so this is a design tradeoff rather than an accident — but the
consequence deserves attention: after a burst of backend errors, an otherwise-idle LB reports
not-ready for as long as it stays idle, and the health checker never resets the breaker
(`internal/health/checker.go:131-154` only touches `Healthy`), so nothing else recovers it. If an
orchestrator gates traffic on `/ready`, the pod can stay out of rotation even though its backends
are fine — the readiness signal and the routing decision disagree.

If `/ready` is meant to mirror "can serve traffic right now", either treat "open but cooldown
elapsed" as ready, or let a successful `/healthz` close the circuit.

### Resolution

The second of the two suggested options was taken, by request: **a healthy `/healthz` now closes the
circuit** (`Backend.ReportHealthy` → `circuitBreaker.recover()`), called from
`internal/health/checker.go` `update` on the *same* hysteresis that sets `Healthy` (2 consecutive
successes), so the health flag and the breaker cannot disagree.

This removes the part of the finding that was a real outage. Before, an idle LB's only route back
was a half-open probe, which requires traffic, so the circuit stayed open and `/ready` stayed `503`
for backends that were answering perfectly. Now recovery is driven out of band, and `/ready` returns
to `200` within at most two checker cycles instead of never.

`recover()` is a no-op in `circuitClosed`, and that restriction is the point: the failure counter
measures request handling, so a health check that cleared it on every cycle would mean a backend
failing one request in five never reached `threshold` and the breaker never fired. Only an actually
open (or half-open) circuit is recovered, and the failure counter is reset only in that case.

`TestCheckerRecoversOpenCircuit` (health, cooldown set to an hour so only the checker can recover)
fails against the previous checker and passes now. `TestCheckerDoesNotResetFailureCounter` pins the
no-op-in-closed half: a successful check landing *between* two failures must not stop the third from
opening the circuit. In `internal/balancer`, `TestCircuitBreakerReportHealthyRecoversOpenCircuit`,
`...LeavesClosedCircuitAlone` and `...RecoversHalfOpen` cover the breaker transitions directly,
including a stalled probe whose late verdict must not re-open a circuit the health check closed.

**What is still open.** The two readings still disagree *at a given instant*, which is the other half
of this finding: `Pool.Ready()` calls `isOpen()`, so between the moment the cooldown elapses and the
next successful health check, `/ready` still says `503` while `Next()` would hand out that backend.
That window is now bounded by the checker interval (≤2 cycles by default) instead of being unbounded,
which is why this is marked partially rather than fully fixed. Closing it completely would mean
`Ready()` treating "open but cooldown elapsed" as ready, which `TestReadyDoesNotTripBreaker`
(`cmd/lb/healthz_test.go:85`) currently forbids — changing that is a separate decision about what
`/ready` is meant to mean, not a bug fix.

---

## 5. [Low] Health checks are sequential, so the effective interval silently stretches

`internal/health/checker.go:81-87` — `checkAll` probes backends one at a time, each with its own
`timeout`. A cycle therefore costs up to `len(backends) * timeout`, and the `time.Ticker` drops
ticks while a cycle is in progress. With the defaults (10s interval, 2s timeout) 5+ backends that
time out push detection of the *last* backend in the list well past 30s, and the configured
`-health-check-every` is no longer the real interval. Bounded parallelism per cycle would keep the
interval meaningful; at minimum this deserves a note in the README next to the flag.

---

## 6. [Low] Unbounded Prometheus label growth via the unauthenticated `/backends` endpoint

`internal/metrics/metrics.go:101-108` keyed on `b.URL.Host`, called from
`internal/proxy/proxy.go:230,237`. Every distinct URL added through `POST /backends` creates new
label values in two `CounterVec`s, and nothing ever deletes them — not even `DELETE /backends`
removes the series. Since the endpoint is unauthenticated by design, a script (or a bored client)
looping over distinct URLs grows registry memory and the `/metrics` payload without bound. Adding
`backendRequests.DeleteLabelValues(...)` / `Delete(prometheus.Labels{...})` on removal, and/or
capping the pool size, would bound it.

---

## 7. [Low] Hardcoded server timeouts are tighter than the documented limits

`cmd/lb/main.go:111-118`

- `ReadTimeout: 5 * time.Second` bounds reading the request *including the body*, while
  `-max-body-bytes` defaults to 1 MiB. A client needs to sustain ~1.7 Mbit/s to finish a default
  upload inside 5s; a slower client gets a truncated request rather than the documented `413`.
- `WriteTimeout: 30 * time.Second` truncates any response longer than 30s. With the shared
  transport's `ResponseHeaderTimeout: 5 * time.Second` (`internal/balancer/transport.go:24`) the
  header budget is much smaller, so a legitimately slow-but-valid response (long-polling, a big
  download) can still be cut at 30s. This also silently bounds retries, since the second attempt
  starts within the same deadline.

Both are hardcoded while every other limit is a flag; consider exposing them (or documenting the
coupling, since `README.md` presents body/concurrency limits as the way oversized traffic is
handled).

---

## 8. [Doc] `README.md` contradicts itself about the request limits

`README.md:144` ("Świadome ograniczenia") still claims:

> **Brak limitu body requestu i limitu równoległych requestów** — obrona przed przeciążeniem
> opiera się na limitach połączeń wspólnego transportu, nie na semaforze aplikacyjnym.

Both limits are implemented (`internal/proxy/proxy.go:46-61`, `46-102`), have flags, are described
in the "Limity" section (`README.md:125-130`), and appear in `examples/lb.yaml`. Only this
limitations bullet is stale; it should be deleted or rewritten.

---

### Notes (not defects)

- `examples/lb.yaml` has no trailing newline.
- The health checker shares the proxy `http.Transport` (`cmd/lb/main.go:92`), so health-check
  traffic consumes the same `MaxConnsPerHost: 100` budget. Harmless today (the checker is
  sequential and closes idle conns), but it couples two concerns that are otherwise independent.
