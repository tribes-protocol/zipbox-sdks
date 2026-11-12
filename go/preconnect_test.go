package zipbox

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Pre-connect on resume (sdks/conformance/README.md, "Pre-connect on resume"): the
// box's last known origin is dialled in parallel with POST /resume on the client's
// pool, so the first box call after it reuses the connection. Never waited for,
// never retried; a different origin in the answer or a failure drops it.

const preconnectExecEvents = "event: start\ndata: {\"id\":\"cmd_1\"}\n\n" +
	"event: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"timedOut\":false}\n\n"

// preconnectBox is a box origin that records its connections and requests.
type preconnectBox struct {
	server      *httptest.Server
	mu          sync.Mutex
	idle        chan struct{}
	healthz     chan struct{}
	healthzAddr string
	execAddr    string
	execAuth    string
	fail        bool
}

func newPreconnectBox(t *testing.T, fail bool) *preconnectBox {
	t.Helper()
	box := &preconnectBox{idle: make(chan struct{}, 8), healthz: make(chan struct{}, 8), fail: fail}
	box.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			box.mu.Lock()
			box.healthzAddr = r.RemoteAddr
			box.mu.Unlock()
			box.healthz <- struct{}{}
			if box.fail {
				if hijacker, ok := w.(http.Hijacker); ok {
					conn, _, _ := hijacker.Hijack()
					_ = conn.Close()
				}
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/exec":
			box.mu.Lock()
			box.execAddr = r.RemoteAddr
			box.execAuth = r.Header.Get("Authorization")
			box.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, preconnectExecEvents)
		default:
			http.NotFound(w, r)
		}
	}))
	box.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			box.idle <- struct{}{}
		}
	}
	box.server.Start()
	t.Cleanup(box.server.Close)
	return box
}

func preconnectDetail(baseURL string, ready bool) map[string]any {
	detail := map[string]any{"id": "sbx", "name": "demo", "status": "running", "ready": ready}
	if ready {
		detail["readiness"] = "ready"
		detail["baseUrl"] = baseURL
		detail["boxToken"] = "bt"
	} else {
		detail["status"] = "paused"
		detail["readiness"] = "paused"
	}
	return detail
}

// preconnectAPI answers GET /sandbox/sbx with the box ready on boxURL, and holds the
// resume until the box has seen the pre-connect, so a pre-connect sent after the
// resume fails the test instead of passing it.
func preconnectAPI(t *testing.T, box *preconnectBox, resumedURL string) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sandbox/sbx/resume":
			select {
			case <-box.healthz:
			case <-time.After(5 * time.Second):
				t.Error("no pre-connect while the resume was in flight")
			}
			_ = json.NewEncoder(w).Encode(preconnectDetail(resumedURL, true))
		case "/sandbox/sbx/pause":
			_ = json.NewEncoder(w).Encode(preconnectDetail("", false))
		default:
			_ = json.NewEncoder(w).Encode(preconnectDetail(box.server.URL, true))
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func TestResumePreconnectsAndTheFirstCallReusesTheConnection(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, false)
	api := preconnectAPI(t, box, box.server.URL)
	client := New("zb_k", WithBaseURL(api.URL))

	if _, err := client.Sandboxes.Get(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	sandbox := client.Sandbox("sbx")
	if _, err := sandbox.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	// The pre-connect's answer is read and its connection back in the pool.
	<-box.idle
	if _, err := sandbox.Exec(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	// The same TCP connection. (Not a connection count: net/http may also dial while a
	// request waits for an idle connection, and pools the spare it did not use.)
	if box.execAddr != box.healthzAddr {
		t.Fatalf("exec did not reuse the pre-connect: exec %s, pre-connect %s",
			box.execAddr, box.healthzAddr)
	}
	if box.execAuth != "Bearer bt" {
		t.Fatalf("exec authorization %q", box.execAuth)
	}
}

func TestStartPreconnectsLikeResume(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, false)
	api := preconnectAPI(t, box, box.server.URL)
	client := New("zb_k", WithBaseURL(api.URL))
	if _, err := client.Sandboxes.Get(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Sandboxes.Start(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
}

func TestAMovedOriginIsReachedTheNormalWay(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, false)
	moved := newPreconnectBox(t, false)
	api := preconnectAPI(t, box, moved.server.URL)
	client := New("zb_k", WithBaseURL(api.URL))

	if _, err := client.Sandboxes.Get(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	sandbox := client.Sandbox("sbx")
	if _, err := sandbox.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Exec(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	moved.mu.Lock()
	defer moved.mu.Unlock()
	if moved.execAuth != "Bearer bt" {
		t.Fatalf("exec did not go to the moved origin")
	}
}

func TestAFailedPreconnectChangesNothing(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, true)
	api := preconnectAPI(t, box, box.server.URL)
	client := New("zb_k", WithBaseURL(api.URL))

	if _, err := client.Sandboxes.Get(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	sandbox := client.Sandbox("sbx")
	if _, err := sandbox.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(ctx, "true")
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code %v", result.ExitCode)
	}
}

func TestAClientThatNeverSawTheBoxReadySendsNone(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, false)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(preconnectDetail(box.server.URL, true))
	}))
	t.Cleanup(api.Close)
	client := New("zb_k", WithBaseURL(api.URL))
	if _, err := client.Sandboxes.Resume(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-box.healthz:
		t.Fatal("a pre-connect without a known origin")
	default:
	}
}
