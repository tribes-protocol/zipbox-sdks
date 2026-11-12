package zipbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A server that closes the connection must end the call with the usual network
// error, never leave it waiting. No timeout makes this true: the close itself does
// (AGENTS.md §0.0 forbids a request deadline on these calls). An exec that may have
// reached the box is never sent again: running a command twice is worse than an error.

// closingServer accepts, reads the request head, writes reply (if any) and closes.
type closingServer struct {
	url      string
	mu       sync.Mutex
	requests []string
}

func (s *closingServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func newClosingServer(t *testing.T, reply string) *closingServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &closingServer{url: "http://" + listener.Addr().String()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				first := ""
				for {
					line, err := reader.ReadString('\n')
					if first == "" {
						first = strings.TrimSpace(line)
					}
					if err != nil || strings.TrimSpace(line) == "" {
						break
					}
				}
				server.mu.Lock()
				server.requests = append(server.requests, first)
				server.mu.Unlock()
				if reply != "" {
					_, _ = conn.Write([]byte(reply))
				}
			}(conn)
		}
	}()
	return server
}

const (
	midBody   = "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 400\r\n\r\n[{\"id\":"
	midEvents = "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\n\r\n40\r\nevent: sta"
)

func assertRetryableNetworkError(t *testing.T, err error) {
	t.Helper()
	var zerr *Error
	if !errors.As(err, &zerr) {
		t.Fatalf("error %T %v, want *Error", err, err)
	}
	if zerr.Code != CodeUnavailable || !zerr.Retryable {
		t.Fatalf("error %+v, want a retryable unavailable", zerr)
	}
}

func TestAConnectionClosedWithNoResponseIsARetryableError(t *testing.T) {
	server := newClosingServer(t, "")
	client := New("zb_k", WithBaseURL(server.url), WithMaxRetries(0))

	_, err := client.Sandboxes.List(context.Background())

	assertRetryableNetworkError(t, err)
}

func TestAConnectionClosedMidBodyIsARetryableError(t *testing.T) {
	server := newClosingServer(t, midBody)
	client := New("zb_k", WithBaseURL(server.url), WithMaxRetries(0))

	_, err := client.Sandboxes.List(context.Background())

	assertRetryableNetworkError(t, err)
}

// boxClient is a client whose mocked control plane places box "sbx" at box.
func boxClient(t *testing.T, box *closingServer, maxRetries int) *Client {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "sbx", "sandboxId": "sbx", "name": "demo", "status": "running",
			"ready": true, "readiness": "ready", "httpsReady": true,
			"baseUrl": box.url, "boxToken": "bt",
		})
	}))
	t.Cleanup(api.Close)
	return New("zb_k", WithBaseURL(api.URL), WithMaxRetries(maxRetries))
}

// execAgainst runs an exec on box through a mocked control plane.
func execAgainst(t *testing.T, box *closingServer) error {
	t.Helper()
	_, err := boxClient(t, box, 3).Sandbox("sbx").Exec(context.Background(), "true")
	return err
}

// A box answer cut mid-body is the same retryable network error the control plane's
// is (the other SDKs read both through one path), never a raw decode error.
func TestABoxAnswerClosedMidBodyIsARetryableError(t *testing.T) {
	box := newClosingServer(t, midBody)

	_, err := boxClient(t, box, 0).Sandbox("sbx").Commands.List(context.Background())

	assertRetryableNetworkError(t, err)
	if seen := box.seen(); len(seen) != 1 {
		t.Fatalf("box saw %q, want exactly one request", seen)
	}
}

func assertExecSentOnce(t *testing.T, box *closingServer, err error) {
	t.Helper()
	var zerr *Error
	if !errors.As(err, &zerr) || zerr.Code != CodeUnavailable {
		t.Fatalf("error %T %v, want an unavailable *Error", err, err)
	}
	if seen := box.seen(); len(seen) != 1 || seen[0] != "POST /exec HTTP/1.1" {
		t.Fatalf("box saw %q, want exactly one POST /exec", seen)
	}
}

func TestAnExecOnAConnectionClosedWithNoResponseIsSentOnce(t *testing.T) {
	box := newClosingServer(t, "")
	err := execAgainst(t, box)
	assertExecSentOnce(t, box, err)
}

func TestAnExecWhoseEventsClosedBeforeItsStartIsSentOnce(t *testing.T) {
	box := newClosingServer(t, midEvents)
	err := execAgainst(t, box)
	assertExecSentOnce(t, box, err)
}
