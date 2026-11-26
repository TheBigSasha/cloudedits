package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerReapsIdleAndSlowHeaderConnections(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadHeaderTimeout > 30*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want a bounded value", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 || srv.IdleTimeout > 5*time.Minute {
		t.Fatalf("IdleTimeout = %v, want a bounded value", srv.IdleTimeout)
	}
}
