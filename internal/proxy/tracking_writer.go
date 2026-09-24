package proxy

import "net/http"

type trackingWriter struct {
	real        http.ResponseWriter
	wroteHeader bool
	err         error
}

func newTrackingWriter(real http.ResponseWriter) *trackingWriter {
	return &trackingWriter{real: real}
}

func (w *trackingWriter) Header() http.Header {
	return w.real.Header()
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.real.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.wroteHeader = true
	return w.real.Write(p)
}

func (w *trackingWriter) UpstreamError(err error) {
	w.err = err
}

func (w *trackingWriter) reset() {
	w.wroteHeader = false
	w.err = nil
}