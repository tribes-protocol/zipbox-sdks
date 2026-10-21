package zipbox

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
