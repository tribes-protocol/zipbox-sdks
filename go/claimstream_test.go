package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Streamed create (sdks/conformance/README.md, "Streamed create"): Create asks for its
// answer streamed and reads the last line's answer exactly like an unstreamed one. The
// placed line is informational: its origin is handed out before its DNS record is known
// to be live, so Create never dials it. An unstreamed answer reads as before.

func claimedDetailBody(baseURL string) map[string]any {
	return map[string]any{
		"sandboxId": "sbx", "name": "demo", "status": "running", "created": true,
		"ready": true, "readiness": "ready", "baseUrl": baseURL, "boxToken": "bt",
	}
}

func writeClaimLine(t *testing.T, w http.ResponseWriter, line map[string]any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(line); err != nil {
		t.Error(err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// claimAPI answers each POST /sandbox/identity/claim with the next of claims, and any
// other path with the box ready on box.
type claimAPI struct {
	server   *httptest.Server
	mu       sync.Mutex
	keys     []string
	accepts  []string
	claims   []func(w http.ResponseWriter)
	fallback map[string]any
}

// noPlacedDial fails the test when anything reached the placed origin's health route.
func noPlacedDial(t *testing.T, box *preconnectBox) {
	t.Helper()
	select {
	case <-box.healthz:
		t.Fatal("create dialled the placed origin")
	default:
	}
}

func newClaimAPI(t *testing.T, fallback map[string]any, claims ...func(w http.ResponseWriter)) *claimAPI {
	t.Helper()
	api := &claimAPI{claims: claims, fallback: fallback}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox/identity/claim" {
			_ = json.NewEncoder(w).Encode(api.fallback)
			return
		}
		api.mu.Lock()
		api.keys = append(api.keys, r.Header.Get("Idempotency-Key"))
		api.accepts = append(api.accepts, r.Header.Get("Accept"))
		next := api.claims[0]
		api.claims = api.claims[1:]
		api.mu.Unlock()
		next(w)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func TestCreateReadsTheLastLineAndNeverDialsThePlacedOrigin(t *testing.T) {
	ctx := context.Background()
	box := newPreconnectBox(t, false)
	detail := claimedDetailBody(box.server.URL)
	api := newClaimAPI(t, detail, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		writeClaimLine(t, w, map[string]any{"type": "placed", "sandboxId": "sbx", "baseUrl": box.server.URL})
		// While the box binds, nothing may reach its origin.
		select {
		case <-box.healthz:
			t.Error("create dialled the placed origin while the box was binding")
		case <-time.After(100 * time.Millisecond):
		}
		writeClaimLine(t, w, map[string]any{"type": "answer", "status": 200, "headers": map[string]string{}, "body": detail})
	})
	client := New("zb_k", WithBaseURL(api.server.URL))

	sandbox, err := client.Sandboxes.Create(ctx, CreateOptions{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	accept := api.accepts[0]
	api.mu.Unlock()
	if !strings.Contains(accept, "application/x-ndjson") {
		t.Fatalf("create did not ask for the stream: %q", accept)
	}
	if _, err := sandbox.Exec(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	noPlacedDial(t, box)
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.execAuth != "Bearer bt" {
		t.Fatalf("exec authorization %q", box.execAuth)
	}
}

func TestCreateReadsAnUnstreamedAnswerAsBefore(t *testing.T) {
	box := newPreconnectBox(t, false)
	detail := claimedDetailBody(box.server.URL)
	api := newClaimAPI(t, detail, func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(detail)
	})
	client := New("zb_k", WithBaseURL(api.server.URL))

	sandbox, err := client.Sandboxes.Create(context.Background(), CreateOptions{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sbx" {
		t.Fatalf("box %q", sandbox.ID)
	}
	noPlacedDial(t, box)
}

func TestCreateRaisesTheErrorInTheLastLine(t *testing.T) {
	box := newPreconnectBox(t, false)
	api := newClaimAPI(t, nil, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeClaimLine(t, w, map[string]any{"type": "placed", "sandboxId": "sbx", "baseUrl": box.server.URL})
		writeClaimLine(t, w, map[string]any{"type": "answer", "status": 409, "headers": map[string]string{}, "body": map[string]any{
			"error": map[string]any{
				"code": "conflict", "message": "Sandbox name is already taken", "requestId": "req_1",
				"retryable": false, "retryAfterMs": nil, "docsUrl": "https://zipbox.ai/docs/errors#conflict",
			},
		}})
	})
	client := New("zb_k", WithBaseURL(api.server.URL))

	_, err := client.Sandboxes.Create(context.Background(), CreateOptions{Name: "demo"})
	var zerr *Error
	if !errors.As(err, &zerr) {
		t.Fatalf("error %v", err)
	}
	if zerr.Code != CodeConflict || zerr.Status != 409 || zerr.RequestID != "req_1" {
		t.Fatalf("error %+v", zerr)
	}
	noPlacedDial(t, box)
}

func TestCreateRetriesAStreamThatEndsBeforeItsAnswerWithTheSameKey(t *testing.T) {
	box := newPreconnectBox(t, false)
	detail := claimedDetailBody(box.server.URL)
	api := newClaimAPI(t, detail,
		func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeClaimLine(t, w, map[string]any{"type": "placed", "sandboxId": "sbx", "baseUrl": box.server.URL})
		},
		func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(detail)
		},
	)
	client := New("zb_k", WithBaseURL(api.server.URL), WithBackoff(time.Millisecond, time.Millisecond))

	if _, err := client.Sandboxes.Create(context.Background(), CreateOptions{Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.keys) != 2 || api.keys[0] != api.keys[1] {
		t.Fatalf("idempotency keys %v", api.keys)
	}
	noPlacedDial(t, box)
}

func TestCreateRetriesAnAnswerLineWithoutAStatusWithTheSameKey(t *testing.T) {
	box := newPreconnectBox(t, false)
	detail := claimedDetailBody(box.server.URL)
	api := newClaimAPI(t, detail,
		func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeClaimLine(t, w, map[string]any{"type": "placed", "sandboxId": "sbx", "baseUrl": box.server.URL})
			writeClaimLine(t, w, map[string]any{"type": "answer", "headers": map[string]string{}})
		},
		func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(detail)
		},
	)
	client := New("zb_k", WithBaseURL(api.server.URL), WithBackoff(time.Millisecond, time.Millisecond))

	if _, err := client.Sandboxes.Create(context.Background(), CreateOptions{Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.keys) != 2 || api.keys[0] != api.keys[1] {
		t.Fatalf("idempotency keys %v", api.keys)
	}
	noPlacedDial(t, box)
}
