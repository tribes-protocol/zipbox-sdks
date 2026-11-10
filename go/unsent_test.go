package zipbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc is an http.RoundTripper from a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIsUnsentOnlyForConnectAndHandshakeFailures(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: "https://box.test", Err: err} }
	unsent := []error{
		wrap(&net.OpError{Op: "dial", Err: errors.New("connection refused")}),
		// The TLS internal alert a fresh box name answers until its certificate lands.
		wrap(&net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}),
		wrap(&net.DNSError{Err: "no such host", Name: "box.test"}),
		wrap(&tls.CertificateVerificationError{Err: errors.New("x509: certificate is valid for other.test")}),
	}
	for _, err := range unsent {
		if !isUnsent(err) {
			t.Errorf("isUnsent(%v) = false, want true", err)
		}
	}
	sent := []error{
		wrap(io.EOF),
		wrap(&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}),
		errors.New("network error"),
	}
	for _, err := range sent {
		if isUnsent(err) {
			t.Errorf("isUnsent(%v) = true, want false", err)
		}
	}
}

// #4139: exec on a box whose HTTPS was not up yet failed at once — a POST was only
// retried when marked safe, and at most maxRetries times. Nothing was sent.
func TestUnsentPostRetriesPastMaxRetriesUntilItConnects(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls <= 4 {
			return nil, &net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	c := New("zb_k", WithHTTPClient(&http.Client{Transport: transport}), WithMaxRetries(0),
		WithBackoff(time.Millisecond, time.Millisecond))
	resp, err := c.do(context.Background(), &request{method: "POST", url: "https://box.test/exec"})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if calls != 5 {
		t.Fatalf("calls = %d, want 5", calls)
	}
}

func TestSentFailureOnPostIsNotRepeated(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}
	})
	c := New("zb_k", WithHTTPClient(&http.Client{Transport: transport}), WithMaxRetries(3),
		WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := c.do(context.Background(), &request{method: "POST", url: "https://box.test/exec"}); !IsCode(err, CodeUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestUnsentRetryEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})
	c := New("zb_k", WithHTTPClient(&http.Client{Transport: transport}),
		WithBackoff(time.Millisecond, time.Millisecond))
	if _, err := c.do(ctx, &request{method: "POST", url: "https://box.test/exec"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// #4137: the old error passed the box's own 401 through as if the key were deleted.
// A deleted key fails the re-read at the control plane; this one passed it.
func TestSecond401SaysTheBoxHasNotReceivedALiveKey(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"sbx_1","name":"demo","status":"running","ready":true,"baseUrl":"`+
			server.URL+`","boxToken":"bt_x"}`)
	})
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	c := New("zb_k", WithBaseURL(server.URL), WithMaxRetries(0))

	err := c.Sandbox("sbx_1").json(context.Background(), "POST", "/exec", nil, map[string]string{}, nil)

	var zerr *Error
	if !errors.As(err, &zerr) || zerr.Code != CodeUnauthorized || !zerr.Retryable {
		t.Fatalf("err = %v, want a retryable unauthorized", err)
	}
	if !strings.Contains(zerr.Message, "has not received") {
		t.Fatalf("message = %q", zerr.Message)
	}
}

// serveTCP accepts connections on a fresh local port and hands each to handle.
func serveTCP(t *testing.T, handle func(net.Conn)) (addr string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			handle(conn)
		}
	}()
	return ln.Addr().String(), accepted
}

// A box name that does not serve yet can accept the connection and close it before
// the TLS handshake. Go then returns a bare io.EOF (or a read reset) from the
// handshake, which isUnsent reads as sent. Nothing was sent: a box POST must reach
// Box.do as unsent, so it re-reads the box.
func TestTLSHandshakeEndedByTheServerIsUnsent(t *testing.T) {
	addr, _ := serveTCP(t, func(conn net.Conn) { conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := jsonBody(map[string]string{"cmd": "true"})
	c := New("zb_k")
	_, err := c.do(ctx, &request{method: "POST", url: "https://" + addr + "/exec", body: body, contentType: "application/json", box: true})
	if zerr, ok := asError(err); !ok || !zerr.unsent {
		t.Fatalf("err = %v, want an unsent failure", err)
	}
}

// The other side of the same line: a server that read the whole request and then
// closed without answering may have run it. That EOF stays sent, and a POST is not
// repeated.
func TestEOFAfterTheRequestWasWrittenIsSent(t *testing.T) {
	addr, accepted := serveTCP(t, func(conn net.Conn) {
		if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
			_, _ = io.Copy(io.Discard, req.Body)
		}
		conn.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := jsonBody(map[string]string{"cmd": "true"})
	c := New("zb_k", WithBackoff(time.Millisecond, time.Millisecond))
	_, err := c.do(ctx, &request{method: "POST", url: "http://" + addr + "/exec", body: body, contentType: "application/json", box: true})
	zerr, ok := asError(err)
	if !ok || zerr.unsent || !IsCode(err, CodeUnavailable) {
		t.Fatalf("err = %v, want a sent unavailable", err)
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("connections = %d, want 1", n)
	}
}

// After a claim a box's baseUrl moves once, from its boot name to its chosen name.
// A box call that cannot reach the cached origin re-reads the box and follows it.
// Each re-read waits one more rung: the ladder grows across re-reads, and a detail
// read that succeeds does not reset it (only an answer from the box would).
func TestBoxUnsentFailureReresolvesOnAGrowingLadder(t *testing.T) {
	dead := deadOrigin(t)
	var live string
	var mu sync.Mutex
	var reads []time.Time
	var auth string
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reads = append(reads, time.Now())
		n := len(reads)
		mu.Unlock()
		base := dead
		if n >= 5 {
			base = live
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"sbx_1","status":"running","readiness":"ready","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-10T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_%d"}`, base, n)
	})
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	live = server.URL
	const base = 10 * time.Millisecond
	c := New("zb_k", WithBaseURL(server.URL), WithBackoff(base, 10*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Sandbox("sbx_1").json(ctx, "POST", "/x", nil, map[string]string{}, nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reads) != 5 {
		t.Fatalf("detail reads = %d, want 5", len(reads))
	}
	if auth != "Bearer bt_5" {
		t.Fatalf("authorization = %q, want the token of the read that named the live origin", auth)
	}
	// The fourth unsent failure sleeps rung 3: at least half of base*2^3. A ladder
	// reset by each read would sleep under base every time.
	if gap := reads[4].Sub(reads[3]); gap < 4*base {
		t.Fatalf("gap before the 5th read = %v, want at least %v", gap, 4*base)
	}
}

// The PTY follows a moved box the same way: a dial that is refused re-reads the
// box with the readiness wait before the next attempt, instead of redialling the
// cached origin forever.
func TestPtyReresolvesAfterARefusedDial(t *testing.T) {
	dead := deadOrigin(t)
	box := &ptyBox{
		refuse: func(int32) int { return 0 },
		serve:  exitAt,
		detail: func(n int32, w http.ResponseWriter) bool {
			if n > 1 {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"sbx_1","status":"running","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-10T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_dead"}`, dead)
			return true
		},
	}
	server := box.start(t)
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pty, err := c.Sandbox("sbx_1").Pty.Create(ctx, PtyOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer pty.Detach()
	if n := box.reads.Load(); n != 2 {
		t.Fatalf("detail reads = %d, want 2", n)
	}
	if n := box.upgrades.Load(); n != 1 {
		t.Fatalf("upgrades = %d, want 1", n)
	}
}

// A refused dial on a box that is now paused ends the terminal with the wait's own
// verdict, as a box call does.
func TestPtyRefusedDialOnAPausedBoxEnds(t *testing.T) {
	dead := deadOrigin(t)
	box := &ptyBox{
		refuse: func(int32) int { return 0 },
		serve:  exitAt,
		detail: func(n int32, w http.ResponseWriter) bool {
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				fmt.Fprintf(w, `{"id":"sbx_1","status":"running","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-10T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_dead"}`, dead)
				return true
			}
			_, _ = io.WriteString(w, `{"id":"sbx_1","status":"archived","phase":"paused","readiness":"paused","createdAt":"2026-11-10T00:00:00Z","updatedAt":"2026-11-10T00:00:00Z","ready":false}`)
			return true
		},
	}
	server := box.start(t)
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Sandbox("sbx_1").Pty.Create(ctx, PtyOptions{})
	var zerr *Error
	if !errors.As(err, &zerr) || zerr.Code != CodeBoxNotRunning || zerr.Retryable {
		t.Fatalf("err = %v, want a non-retryable box_not_running", err)
	}
	if n := box.upgrades.Load(); n != 0 {
		t.Fatalf("upgrades = %d, want 0", n)
	}
}
