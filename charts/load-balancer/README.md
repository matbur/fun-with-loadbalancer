# Helm chart for the load balancer

Deploys `cmd/lb` as a Deployment behind a Service, with its configuration in a
ConfigMap. With no values at all it also creates two `traefik/whoami` pods and
routes to them, the k8s counterpart of the two whoami services in
`docker-compose.yml`.

```bash
helm install lb charts/load-balancer
```

The load balancer image is `fun-with-loadbalancer:dev` by default, which does not
exist in any registry. Point `image.repository` at your own registry, or load
the locally built image into the cluster - note that a plain `docker build` is
not enough on Docker Desktop, whose Kubernetes node resolves images through a
pull-through mirror and does not see the host's local image store:

```bash
make docker-build
kind load docker-image fun-with-loadbalancer:dev   # kind
minikube image load fun-with-loadbalancer:dev      # minikube
```

Then:

```bash
kubectl port-forward svc/lb-load-balancer 8080:8080
open http://localhost:8080/admin
helm test lb
```

## Configuration

The binary reads a config file (`-config`), and the file format is the one
documented in `examples/lb.yaml` - same keys, same meaning. A ConfigMap is used
instead of flags because the file carries lists (`backends`, `weights`) that the
flags can only express as comma-separated strings.

```bash
helm install lb charts/load-balancer \
  --set 'config.backends={http://orders.default.svc.cluster.local:8080,http://payments.default.svc.cluster.local:8080}' \
  --set config.algorithm=weighted-round-robin
```

Every key lives under `config` - `--set algorithm=...` at the top level is
silently ignored, which is a mistake worth naming. Set a value to `null` to
leave it out of the file, and the binary's own default applies. `config.extra`
passes through keys the chart does not render, which keeps working when the
binary grows a new option. A change to the ConfigMap is hashed into the pod
template, so a config change rolls the Deployment instead of silently doing
nothing.

## Values worth knowing

| Key | Default | Why |
| --- | --- | --- |
| `config.backends` | `[]` | Empty falls back to `demoBackends`. At least one backend is required - the chart fails at render time rather than leaving a pod in `CrashLoopBackOff`. |
| `config.listen` | `":8080"` | Also the source of `containerPort`, so the two cannot drift. |
| `config.log_format` | `json` | Not the binary's `text` default: cluster log collectors parse JSON. |
| `config.max_body_bytes` | `1048576` | Rendered as an integer on purpose - see the note below. |
| `demoBackends.enabled` | `true` | Set to `false` together with `config.backends` for real traffic. |
| `replicaCount` | `1` | Each replica runs its own pool, health checks and circuit breakers. The Service balances per connection, so weights apply within a replica, not across the fleet. Scale for availability, not for balance. |
| `terminationGracePeriodSeconds` | `30` | The process drains for up to 10s (`shutdownTimeout` in `cmd/lb/main.go`). Below that, `SIGKILL` cuts the drain and clients see `502`. |
| `serviceAccount.automountServiceAccountToken` | `false` | The load balancer never calls the Kubernetes API. |
| `podSecurityContext` / `containerSecurityContext` | see `values.yaml` | Mirrors compose: non-root, read-only root filesystem, all capabilities dropped. |

## Probes

`/live` for liveness - it reports that the process runs, nothing more.
`/ready` for readiness - it reports whether at least one backend can take
traffic, so a pod with all backends down is removed from the Service instead of
answering `503` to clients.

A fresh pod is Ready immediately - backends start marked healthy (`Healthy =
true` in `internal/balancer`), so `/ready` answers `200` from process start.
Readiness is optimistic at startup and only bites once the checker has seen
three consecutive failed cycles (`failureThreshold = 3`), about 30 seconds with
the default `health_check_every: 10s`. The two-successes threshold applies to
*recovery*: a backend the checker has marked unhealthy needs two consecutive
passing probes before `/ready` turns `200` again.

## Deliberate omissions

- **No `preStop` hook.** The image is distroless: no shell, no `sleep`, so there
  is nothing to run. Endpoint removal and `SIGTERM` happen concurrently, so a
  client reusing a keep-alive connection can still have a request cut - the
  trade is made on purpose, in exchange for not shipping a shell.
- **No PodDisruptionBudget.** The load balancer is stateless and the Deployment
  already spreads replicas if you set `topologySpreadConstraints`.
- **No ServiceMonitor.** `/metrics` is on the same port as everything else;
  scrape it with annotations on the Service, or add a ServiceMonitor yourself.
