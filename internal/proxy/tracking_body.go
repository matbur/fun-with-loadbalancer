package proxy

import (
	"errors"
	"io"
)

// trackingBody wraps the client request body and remembers the first read
// error. ReverseProxy copies the body to the backend, so a body that never
// arrives surfaces as an upstream transport error — indistinguishable from a
// backend that is broken, and charged to it. Recording the read error here is
// what tells the two apart. EOF is ignored: a body that ends is not a failure.
type trackingBody struct {
	body    io.ReadCloser
	readErr error
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.readErr == nil {
		b.readErr = err
	}
	return n, err
}

func (b *trackingBody) Close() error {
	return b.body.Close()
}
