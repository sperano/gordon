package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server timeouts of the public receiver, the legacy bounded_http limits:
// a head must arrive quickly, and the whole request within the deadline.
const (
	readHeaderTimeout = 10 * time.Second
	maxHeaderBytes    = 64 << 10
	idleTimeout       = 60 * time.Second
	writeSlack        = 10 * time.Second
	// adminWriteTimeout bounds a probe or a scrape that stops reading.
	adminWriteTimeout = 30 * time.Second
	// adminReadTimeout bounds a request body nobody reads (the admin
	// endpoints are GET only).
	adminReadTimeout = 10 * time.Second
)

// serve runs srv on ln until ctx ends, then shuts it down, letting
// requests in flight finish for up to grace.
func serve(ctx context.Context, log *slog.Logger, name string, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "server", name, "addr", ln.Addr().String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if serr := <-errc; !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, serr)
	}
	log.Info("stopped", "server", name)
	return err
}

func adminServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout, MaxHeaderBytes: maxHeaderBytes,
		ReadTimeout: adminReadTimeout, WriteTimeout: adminWriteTimeout, IdleTimeout: idleTimeout}
}

func listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
