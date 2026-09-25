package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dto "github.com/prometheus/client_model/go"
)

func countFor(m *Metrics, method, status string) float64 {
	return testutil.ToFloat64(m.requestsTotal.WithLabelValues(method, status))
}

func TestWrapRecordsStatusAndCount(t *testing.T) {
	m := New()

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, 1.0, countFor(m, http.MethodGet, "418"))
	assert.Equal(t, 0.0, countFor(m, http.MethodGet, "200"))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.requestsInFlight))

	metric, ok := m.requestDuration.WithLabelValues(http.MethodGet).(prometheus.Metric)
	require.True(t, ok)
	var dto dto.Metric
	require.NoError(t, metric.Write(&dto))
	assert.Equal(t, uint64(1), dto.Histogram.GetSampleCount())
}

func TestWrapDefaultsStatusTo200(t *testing.T) {
	m := New()

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	assert.Equal(t, 200, rec.Code)
	assert.Equal(t, 1.0, countFor(m, http.MethodPost, "200"))
}

func TestWrapTracksConcurrentRequestsInFlight(t *testing.T) {
	m := New()
	enter := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(enter)
		<-release
	}))

	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()

	<-enter
	assert.Equal(t, 1.0, testutil.ToFloat64(m.requestsInFlight))

	close(release)
	<-done
	assert.Equal(t, 0.0, testutil.ToFloat64(m.requestsInFlight))
}

func TestWrapPreservesStreaming(t *testing.T) {
	m := New()

	streamed := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		require.True(t, ok, "Flusher must survive the metrics middleware")
		f.Flush()
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	streamed.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBackendCounters(t *testing.T) {
	m := New()

	m.ObserveBackendRequest("127.0.0.1:9001")
	m.ObserveBackendRequest("127.0.0.1:9001")
	m.ObserveBackendRequest("127.0.0.1:9002")

	assert.Equal(t, 2.0, testutil.ToFloat64(m.backendRequests.WithLabelValues("127.0.0.1:9001")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.backendRequests.WithLabelValues("127.0.0.1:9002")))

	m.ObserveBackendError("127.0.0.1:9001")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.backendErrors.WithLabelValues("127.0.0.1:9001")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.backendErrors.WithLabelValues("127.0.0.1:9002")))
}

func TestMetricsEndpointServesPrometheusText(t *testing.T) {
	m := New()

	// feed one request so the endpoint has something to report
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	m.ObserveBackendError("127.0.0.1:9001")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain"))

	body := rec.Body.String()
	assert.Contains(t, body, "# HELP lb_http_requests_total")
	assert.Contains(t, body, `lb_http_requests_total{method="GET",status="404"} 1`)
	assert.Contains(t, body, "lb_backend_errors_total")
}