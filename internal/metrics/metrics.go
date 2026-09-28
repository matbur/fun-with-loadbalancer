// Package metrics exposes Prometheus metrics for the load balancer.
package metrics

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics aggregates the Prometheus collectors exposed by the load balancer.
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight prometheus.Gauge

	backendRequests *prometheus.CounterVec
	backendErrors   *prometheus.CounterVec
}

// New creates and registers all metric collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "lb",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total number of HTTP requests handled by the load balancer, by method and response status.",
		}, []string{"method", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "lb",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "Duration of HTTP requests handled by the load balancer, in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method"}),
		requestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "lb",
			Subsystem: "http",
			Name:      "requests_in_flight",
			Help:      "Number of HTTP requests currently being handled.",
		}),
		backendRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "lb",
			Subsystem: "backend",
			Name:      "requests_total",
			Help:      "Total number of attempts routed to a backend, by backend host.",
		}, []string{"backend"}),
		backendErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "lb",
			Subsystem: "backend",
			Name:      "errors_total",
			Help:      "Total number of transport-level errors when contacting a backend, by backend host.",
		}, []string{"backend"}),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.requestsInFlight,
		m.backendRequests,
		m.backendErrors,
	)

	return m
}

// Handler returns an HTTP handler serving the registered metrics in the
// Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Wrap returns a middleware that records per-request metrics (response status,
// duration, in-flight requests) around h. The response status is captured
// without buffering: [statusWriter] only observes the first WriteHeader/Write.
func (m *Metrics) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestsInFlight.Inc()
		defer m.requestsInFlight.Dec()

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)

		m.requestsTotal.WithLabelValues(r.Method, strconv.Itoa(sw.statusCode())).Inc()
		m.requestDuration.WithLabelValues(r.Method).Observe(time.Since(start).Seconds())
	})
}

// ObserveBackendRequest counts an attempt routed to backend (identified by its host).
func (m *Metrics) ObserveBackendRequest(backend string) {
	m.backendRequests.WithLabelValues(backend).Inc()
}

// ObserveBackendError counts a transport-level error for backend (identified by its host).
func (m *Metrics) ObserveBackendError(backend string) {
	m.backendErrors.WithLabelValues(backend).Inc()
}

// BackendRequests returns the current number of attempts routed to a backend
// host (0 if none yet).
func (m *Metrics) BackendRequests(backend string) float64 {
	return m.counterValue("lb_backend_requests_total", "backend", backend)
}

// BackendErrors returns the current number of transport-level errors for a
// backend host (0 if none yet).
func (m *Metrics) BackendErrors(backend string) float64 {
	return m.counterValue("lb_backend_errors_total", "backend", backend)
}

// ForgetBackend drops every series keyed by backend, so a removed backend stops
// showing up in /metrics. Label values are only ever added, so without this the
// registry grows for the life of the process.
//
// Partial: covers add-then-remove, not a client that only adds. That needs a
// pool size cap or authentication.
func (m *Metrics) ForgetBackend(backend string) {
	m.backendRequests.DeleteLabelValues(backend)
	m.backendErrors.DeleteLabelValues(backend)
}

func (m *Metrics) counterValue(name, label, value string) float64 {
	families, err := m.registry.Gather()
	if err != nil {
		return 0
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, mv := range f.GetMetric() {
			for _, lp := range mv.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					return mv.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// statusWriter captures the first response status written to the underlying
// ResponseWriter without buffering the response body. It forwards optional
// ResponseWriter capabilities (Flusher, Hijacker, Pusher) and exposes Unwrap
// for http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) statusCode() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.status
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}