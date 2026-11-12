package zipbox

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ptyBox serves a ready box sbx_1. Its terminal answers the n-th upgrade (from 1)
// with refuse(n) when that is non-zero; otherwise it accepts and hands the socket
// to serve. GET /sandbox/sbx_1 answers detail(n) for the n-th read.
type ptyBox struct {
	refuse   func(n int32) int
	serve    func(conn *websocket.Conn, n int32)
	detail   func(n int32, w http.ResponseWriter) bool
	upgrades atomic.Int32
	reads    atomic.Int32
}

func (b *ptyBox) start(t *testing.T) *httptest.Server {
	var base string
	mux := http.NewServeMux()
	ready := detailHandler(&base, "bt_pty")
	// A detail read and a readiness wait answer the same row.
	read := func(w http.ResponseWriter, r *http.Request) {
		n := b.reads.Add(1)
		if b.detail != nil && b.detail(n, w) {
			return
		}
		ready(w, r)
	}
	mux.HandleFunc("/sandbox/sbx_1", read)
	mux.HandleFunc("/sandbox/sbx_1/wait", read)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := b.upgrades.Add(1)
		if status := b.refuse(n); status != 0 {
			w.WriteHeader(status)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ptySubprotocol}})
		if err != nil {
			return
		}
		b.serve(conn, n)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base = server.URL
	return server
}

func exitAt(conn *websocket.Conn, _ int32) {
	conn.Write(context.Background(), websocket.MessageText, []byte(`{"t":"x","code":0}`))
	conn.Close(websocket.StatusNormalClosure, "")
}

// dropAfterOutput sends "a" on the first socket and drops it; later sockets exit.
func dropAfterOutput(conn *websocket.Conn, n int32) {
	if n == 1 {
		conn.Write(context.Background(), websocket.MessageText, []byte(`{"t":"o","d":"a"}`))
		conn.CloseNow()
		return
	}
	exitAt(conn, n)
}

// AX-07: a box is ready before its certificate lands, so the first terminal dial
// can fail its TLS handshake. Nothing was sent; the dial waits it out on the PTY
// ladder like every later reconnect, instead of failing `zipbox shell`.
func TestPtyFirstDialWaitsOutAFailedTLSHandshake(t *testing.T) {
	box := &ptyBox{refuse: func(int32) int { return 0 }, serve: exitAt}
	server := box.start(t)
	var handshakes atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/" && handshakes.Add(1) <= 2 {
			return nil, &net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := New("zb_x", WithBaseURL(server.URL), WithHTTPClient(&http.Client{Transport: transport}),
		WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	pty, err := c.Sandbox("sbx_1").Pty.Create(context.Background(), PtyOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer pty.Detach()
	if n := handshakes.Load(); n != 3 {
		t.Fatalf("handshakes = %d, want 3", n)
	}
}

// A box that drops the upgrade after it was sent (its proxy reloading as the
// certificate lands) gave no answer either. The upgrade names its session and client,
// so repeating it is safe: the first dial waits it out, as a reconnect already does.
func TestPtyFirstDialWaitsOutADroppedUpgrade(t *testing.T) {
	box := &ptyBox{refuse: func(int32) int { return 0 }, serve: exitAt}
	server := box.start(t)
	var handshakes atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/" && handshakes.Add(1) == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := New("zb_x", WithBaseURL(server.URL), WithHTTPClient(&http.Client{Transport: transport}),
		WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	pty, err := c.Sandbox("sbx_1").Pty.Create(context.Background(), PtyOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer pty.Detach()
	if n := handshakes.Load(); n != 2 {
		t.Fatalf("handshakes = %d, want 2", n)
	}
}

// The first-dial wait has no ceiling: the caller's context is its only bound.
func TestPtyFirstDialWaitEndsWithTheContext(t *testing.T) {
	box := &ptyBox{refuse: func(int32) int { return 0 }, serve: exitAt}
	server := box.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	var handshakes atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/" {
			if handshakes.Add(1) == 3 {
				cancel()
			}
			return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := New("zb_x", WithBaseURL(server.URL), WithHTTPClient(&http.Client{Transport: transport}),
		WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	if _, err := c.Sandbox("sbx_1").Pty.Create(ctx, PtyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// AX-14: a box that still answers 401 after the control plane re-read accepted the
// key has not received a key created moments ago. That is no verdict: the first
// dial waits for it.
func TestPtyFirstDialWaitsForTheBoxToReceiveALiveKey(t *testing.T) {
	box := &ptyBox{
		refuse: func(n int32) int {
			if n <= 2 {
				return http.StatusUnauthorized
			}
			return 0
		},
		serve: exitAt,
	}
	server := box.start(t)
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	pty, err := c.Sandbox("sbx_1").Pty.Create(context.Background(), PtyOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer pty.Detach()
	if n := box.upgrades.Load(); n != 3 {
		t.Fatalf("upgrades = %d, want 3", n)
	}
}

// AX-14: the same on a reconnect. Each attempt re-reads the token, and the session
// goes on once the box has the key.
func TestPtyReconnectWaitsForTheBoxToReceiveALiveKey(t *testing.T) {
	box := &ptyBox{
		refuse: func(n int32) int {
			if n >= 2 && n <= 5 {
				return http.StatusUnauthorized
			}
			return 0
		},
		serve: dropAfterOutput,
	}
	server := box.start(t)
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	pty, err := c.Sandbox("sbx_1").Pty.Create(context.Background(), PtyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Detach()
	out, err := io.ReadAll(pty)
	if err != nil || string(out) != "a" {
		t.Fatalf("read %q, %v; want the session to go on to its exit", out, err)
	}
	if n := box.upgrades.Load(); n != 6 {
		t.Fatalf("upgrades = %d, want 6", n)
	}
	// Once the box has refused a re-read token, every ladder step re-reads it and
	// dials once, as in Python and Rust: the first dial plus four steps.
	if n := box.reads.Load(); n != 5 {
		t.Fatalf("token reads = %d, want 5", n)
	}
}

// AX-14: a deleted key fails the re-read at the control plane. That is the answered
// verdict, and it ends the terminal.
func TestPtyEndsWhenTheControlPlaneRefusesTheKey(t *testing.T) {
	box := &ptyBox{
		refuse: func(n int32) int {
			if n >= 2 {
				return http.StatusUnauthorized
			}
			return 0
		},
		serve: dropAfterOutput,
		detail: func(n int32, w http.ResponseWriter) bool {
			if n == 1 {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"Invalid or deleted API key","requestId":"r","retryable":false}}`)
			return true
		},
	}
	server := box.start(t)
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	pty, err := c.Sandbox("sbx_1").Pty.Create(context.Background(), PtyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Detach()
	out, err := io.ReadAll(pty)
	if string(out) != "a" || !IsCode(err, CodeUnauthorized) {
		t.Fatalf("read %q, %v; want the output, then unauthorized", out, err)
	}
}
