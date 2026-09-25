package proxy

import (
	"bufio"
	"net"
	"net/http"
)

// trackingWriter wraps the real ResponseWriter so the proxy can observe
// whether anything was written to the client and whether the upstream failed
// before retrying. It forwards optional ResponseWriter capabilities (Flusher,
// Hijacker) and exposes Unwrap for http.ResponseController, so wrapping does
// not break streaming responses.
type trackingWriter struct {
	real        http.ResponseWriter
	wroteHeader bool
	err         error
	status      int
}

func newTrackingWriter(real http.ResponseWriter) *trackingWriter {
	return &trackingWriter{real: real}
}

func (w *trackingWriter) Header() http.Header {
	return w.real.Header()
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.status = code
	w.real.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.wroteHeader = true
	return w.real.Write(p)
}

func (w *trackingWriter) UpstreamError(err error) {
	w.err = err
}

func (w *trackingWriter) Flush() {
	if f, ok := w.real.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.real.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

func (w *trackingWriter) Unwrap() http.ResponseWriter {
	return w.real
}

func (w *trackingWriter) reset() {
	w.wroteHeader = false
	w.err = nil
	w.status = 0
}