# Future plan

Ideas that were raised and not implemented, with the reasoning kept so the
decision does not have to be re-derived from scratch. Nothing here is a promise.

## 1. SSE for the admin panel — built, measured, reverted

**Motivation.** The panel polls `GET /backends` every 2 s to decide whether a
backend went unhealthy. Status changes are rare; the poll exists only because
there is no way to learn about them without asking.

**What was built.** A `/admin/events` SSE stream pushed a full pool snapshot
whenever state changed, and the panel switched from `setInterval` to
`EventSource`. The whole thing was written and then reverted on request; the
server side is back to byte-identical with the pre-SSE state, so nothing of it
survives.

**How state changes were detected.** Three hook points, none of which needed a
change inside `internal/balancer`:

- `health.Checker.update` — already ran per verdict; change detection was
  `b.Healthy.Swap(true/false)`, whose return value is the previous state, so
  a steady pool stayed quiet instead of notifying once per probe per cycle.
- `proxy.Server.ServeHTTP` — `defer` registered as the first statement, so it
  runs *last* and the in-flight slot for the backend is already back when the
  panel is told the count moved.
- `admin.add` / `admin.remove` — after a successful pool mutation.

**The part that is not simple.** "No polling at all" is not achievable while the
panel shows the in-flight count. That column changes on every request, so a
truly event-driven stream is one event per request under load. The design that
worked needed two extra pieces: a rate floor (pushes coalesced to at most one per
250 ms, which is what turns a burst of requests into a single push) and a
keepalive comment every 15 s so an idle stream is not dropped in between. Both
are easy to get subtly wrong, and the failure mode is a stream that silently
stops updating.

**Why it was not kept.** One small JSON array every 2 s, from one open browser
tab, is not worth a long-lived connection, a coalescing timer to get right, and a
second public endpoint. The trigger to revisit: the pool grows past a size where
polling the whole array stops being cheap, or more than one operator watches the
panel at once.

**If it is revisited.** The hook points above are verified to work. The
unsolved part is the rate floor and what to do with the volatile `active`
column — dropping it from the stream and leaving it on a slow poll is probably
more honest than pretending it is state.

## 2. `templ` for the admin panel HTML

**Motivation.** `cmd/lb/web/admin.html` is 440 lines of HTML, CSS and JS in one
embedded blob. It works and it is deliberately dependency-free, but there is no
structure in it: no layout component, no template at all. The table rows are
built imperatively in JS, and the page is untyped end to end.

**What `templ` would change.** `github.com/a-h/templ` compiles `.templ` files
into type-safe Go, so the markup becomes a normal Go function with checked
parameters. The backend table becomes a component taking `[]backendInfo` instead
of a loop in a script tag. `//go:embed` stays for the CSS and the JS — those
are static assets and templ has no opinion about them.

**What it costs.** A code-generation step and a build dependency in a project
that currently has neither. `templ generate` has to run before `go build`, which
means a `go:generate` hook, a Makefile target, or a CI step — all new moving
parts for one page.

**The honest sizing.** templ earns its keep with shared layouts, several pages,
or templates rendered server-side. This is one page, one table, no server-side
rendering. The payoff is real but small, and the cost is a build step that every
future contributor has to know about.

**The trigger to revisit.** A second admin page, or a shared layout appearing
across pages. If the only remaining pain is "the rows are built imperatively",
templ is the wrong fix — that is a function, not a template.

## Related, still open

- The request console in the panel does not show which backend served the
  request; that identity is known only to the access log. Showing it needs a
  response header from the proxy, which changes the proxy's response for all
  traffic, not just the console.
- An add-then-remove cycle only drops its metric series (`Metrics.ForgetBackend`
  covers it). A client that only ever adds still grows label values, because
  `/backends` is unauthenticated. Closing that needs a pool size cap or
  authentication — the cap was written and removed on request.
