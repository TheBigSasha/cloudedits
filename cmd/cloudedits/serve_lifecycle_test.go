package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type gatedIndexer struct {
	scanStarted chan struct{}
	scanRelease chan struct{}
	scanDone    atomic.Bool
	watching    atomic.Bool
}

func newGatedIndexer() *gatedIndexer {
	return &gatedIndexer{scanStarted: make(chan struct{}), scanRelease: make(chan struct{})}
}

func (g *gatedIndexer) ScanAll(ctx context.Context) error {
	close(g.scanStarted)
	select {
	case <-g.scanRelease:
		g.scanDone.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedIndexer) StartWatcher(context.Context) error {
	g.watching.Store(true)
	return nil
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// TestServeAnswersWhileInitialScanIsStillRunning pins the startup budget: the
// listener must accept requests before the library walk finishes, however
// long that walk takes.
func TestServeAnswersWhileInitialScanIsStillRunning(t *testing.T) {
	ln := listenLoopback(t)
	idx := newGatedIndexer()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntilDone(ctx, ln, handler, idx, true, t.Logf) }()

	select {
	case <-idx.scanStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial scan never started")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/capabilities")
	if err != nil {
		t.Fatalf("request during initial scan failed: %v", err)
	}
	_ = resp.Body.Close()
	if idx.scanDone.Load() {
		t.Fatal("test setup: scan finished before the request was answered")
	}
	if idx.watching.Load() {
		t.Fatal("watcher started before the initial scan finished")
	}

	close(idx.scanRelease)
	deadline := time.Now().Add(5 * time.Second)
	for !idx.watching.Load() {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not start after the initial scan")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveUntilDone: %v", err)
	}
}

// TestServeDrainsInFlightRequestBeforeReturning pins the shutdown order: the
// caller closes the index right after serveUntilDone returns, so a request
// that is still executing must have finished by then.
func TestServeDrainsInFlightRequestBeforeReturning(t *testing.T) {
	ln := listenLoopback(t)
	idx := newGatedIndexer()
	close(idx.scanRelease)

	inHandler := make(chan struct{})
	release := make(chan struct{})
	var handlerFinished atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		<-release
		handlerFinished.Store(true)
		_, _ = io.WriteString(w, "written after shutdown began")
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntilDone(ctx, ln, handler, idx, false, t.Logf) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/ops")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()

	<-inHandler
	cancel()
	select {
	case err := <-done:
		t.Fatalf("serveUntilDone returned (%v) while a request was still executing", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveUntilDone: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilDone did not return after the request finished")
	}
	if !handlerFinished.Load() {
		t.Fatal("serveUntilDone returned before the in-flight handler finished")
	}
	if got := <-body; got != "written after shutdown began" {
		t.Fatalf("in-flight response = %q", got)
	}
}
