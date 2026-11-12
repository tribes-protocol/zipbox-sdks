// Package zipbox is the Go SDK for Zipbox: create boxes, run commands, move
// files, open terminals and publish ports from code.
//
//	c := zipbox.New("") // reads ZIPBOX_API_KEY
//	box, err := c.Sandboxes.GetOrCreate(ctx, "demo", zipbox.CreateOptions{Tier: "s"})
//	res, err := box.Exec(ctx, "echo hello")
//	fmt.Print(res.Stdout)
//
// Nothing on a box stops on a clock, and no call has a default deadline: pass a
// context with a deadline when you want one.
package zipbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the SDK version, sent in the User-Agent header.
const Version = "0.2.1"

// DefaultBaseURL is the Zipbox control plane.
const DefaultBaseURL = "https://api.tribes.xyz"

// Client is a Zipbox API client. It is safe for concurrent use. Box tokens are
// fetched lazily per box and cached for the client's lifetime.
type Client struct {
	// Sandboxes creates, lists and manages boxes.
	Sandboxes *SandboxesService
	// Account reads the account's balance and usage.
	Account *AccountService

	apiKey     string
	baseURL    string
	httpClient *http.Client
	maxRetries int

	backoffBase time.Duration
	backoffMax  time.Duration
	pollMin     time.Duration
	pollMax     time.Duration
	ptyBase     time.Duration
	ptyMax      time.Duration

	privateProbeTimeout time.Duration

	mu    sync.Mutex
	creds map[string]*boxCreds
	// lastOrigins is the last target each box answered on, kept across forget: a
	// paused box's detail names none, and its resume pre-connects to the one it had.
	lastOrigins map[string]*boxCreds
	rnd         *rand.Rand
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the control plane URL (default ZIPBOX_API_URL or DefaultBaseURL).
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient sets the HTTP client. The default has no timeout.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) { c.httpClient = httpClient }
}

// WithMaxRetries sets how many times a retryable failure (rate_limited,
// unavailable) of a GET or a create is retried. Default 3.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithBackoff sets the exponential backoff used between retries and command
// stream reconnects: the first delay and the cap. Default 500ms and 30s.
func WithBackoff(base, max time.Duration) Option {
	return func(c *Client) { c.backoffBase, c.backoffMax = base, max }
}

// WithPollInterval sets how often Create re-reads a box while it boots: the
// first delay and the cap. Default 1s and 10s; each delay grows 1.25x, with jitter.
func WithPollInterval(min, max time.Duration) Option {
	return func(c *Client) { c.pollMin, c.pollMax = min, max }
}

// WithPtyBackoff sets the backoff between PTY reconnects: the first delay and
// the cap. Default 500ms and 30s. There is no attempt ceiling.
func WithPtyBackoff(base, max time.Duration) Option {
	return func(c *Client) { c.ptyBase, c.ptyMax = base, max }
}

// New returns a client. An empty apiKey reads ZIPBOX_API_KEY.
func New(apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		apiKey = os.Getenv("ZIPBOX_API_KEY")
	}
	baseURL := os.Getenv("ZIPBOX_API_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		apiKey:      apiKey,
		baseURL:     strings.TrimRight(baseURL, "/"),
		httpClient:  &http.Client{},
		maxRetries:  3,
		backoffBase: 500 * time.Millisecond,
		backoffMax:  30 * time.Second,
		pollMin:     time.Second,
		pollMax:     10 * time.Second,
		ptyBase:     500 * time.Millisecond,
		ptyMax:      30 * time.Second,
		creds:       map[string]*boxCreds{},
		lastOrigins: map[string]*boxCreds{},
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())),

		privateProbeTimeout: defaultPrivateProbeTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.Sandboxes = &SandboxesService{client: c}
	c.Account = &AccountService{client: c}
	return c
}

// request is one HTTP call. body is a factory so a retried request re-sends the
// same bytes.
type request struct {
	method      string
	url         string
	query       url.Values
	body        func() (io.Reader, error)
	contentType string
	headers     map[string]string
	// retry marks the request safe to repeat on a retryable error: GETs, and a
	// create that carries an Idempotency-Key.
	retry bool
	// box marks a box-plane request: an unsent failure is returned to Box.do, which
	// re-reads the box before it retries, since a box's origin can move.
	box bool
	// unwrap turns the response into the one the request answers before its status is
	// read: a streamed create's last line stands for its answer. Its error is a sent
	// failure, retried like a dropped connection.
	unwrap func(*http.Response) (*http.Response, error)
}

func jsonBody(v any) (func() (io.Reader, error), error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return func() (io.Reader, error) { return bytes.NewReader(data), nil }, nil
}

// do sends req, retrying retryable failures when req.retry is set. It returns
// a 2xx response with its body open, or an error (a *Error for HTTP failures).
//
// A failure to connect or to complete the TLS handshake is retried for EVERY
// method with no attempt ceiling: nothing was sent. The ladder is capped and
// jittered; ctx is the only bound. A box request returns that failure instead:
// Box.do re-reads the box, then retries.
func (c *Client) do(ctx context.Context, req *request) (*http.Response, error) {
	unsentAttempt := 0
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, req)
		if err == nil {
			return resp, nil
		}
		zerr, ok := asError(err)
		if ok && zerr.unsent {
			if req.box {
				return nil, err
			}
			if err := sleep(ctx, c.backoff(unsentAttempt)); err != nil {
				return nil, err
			}
			unsentAttempt++
			attempt--
			continue
		}
		if !req.retry || !ok || attempt >= c.maxRetries || !zerr.Retryable {
			return nil, err
		}
		wait := c.backoff(attempt)
		if zerr.hasRetryAfter {
			wait = zerr.RetryAfter
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *Client) once(ctx context.Context, req *request) (*http.Response, error) {
	target := req.url
	if len(req.query) > 0 {
		target += "?" + req.query.Encode()
	}
	var body io.Reader
	if req.body != nil {
		b, err := req.body()
		if err != nil {
			return nil, err
		}
		body = b
	}
	var sent sendTrace
	httpReq, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, sent.hooks()), req.method, target, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", "zipbox-sdk-go/"+Version)
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		zerr := newError(CodeUnavailable, err.Error())
		zerr.unsent = sent.unsent(err)
		return nil, zerr
	}
	if req.unwrap != nil {
		resp, err = req.unwrap(resp)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, newError(CodeUnavailable, err.Error())
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return nil, errorFromResponse(resp, data)
}

// sendTrace records, from the standard transport's own trace, whether the request's
// headers were written. Each connection attempt (GetConn) starts over, so it
// describes the attempt the error came from.
type sendTrace struct {
	attempted atomic.Bool
	wrote     atomic.Bool
}

func (t *sendTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			t.attempted.Store(true)
			t.wrote.Store(false)
		},
		WroteHeaders: func() { t.wrote.Store(true) },
	}
}

// unsent reports whether a failed request never reached the server. A traced
// attempt that failed before its headers were written was not sent: a connect, DNS
// or TLS handshake failure, including a handshake the server ended at once (Go
// returns a bare io.EOF or a read reset then). Anything after the headers counts as
// sent. A transport that reports no trace falls back to isUnsent.
func (t *sendTrace) unsent(err error) bool {
	if t.attempted.Load() {
		return !t.wrote.Load()
	}
	return isUnsent(err)
}

// isUnsent reports whether a transport error provably happened before the request
// left: a failed dial (DNS, refusal, unreachable), a TLS alert from the peer during
// the handshake (Go reports it as a "remote error" OpError), or a certificate the
// handshake could not verify. A reset or EOF is not: those can follow a sent request.
func isUnsent(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "remote error") {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return true
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// api calls the control plane with the account key.
func (c *Client) api(ctx context.Context, method, path string, in any, out any, retry bool, headers map[string]string) error {
	return c.apiUnwrap(ctx, method, path, in, out, retry, headers, nil)
}

// apiUnwrap is api with an unwrap step (see request.unwrap).
func (c *Client) apiUnwrap(ctx context.Context, method, path string, in any, out any, retry bool, headers map[string]string, unwrap func(*http.Response) (*http.Response, error)) error {
	if c.apiKey == "" {
		return newError(CodeUnauthorized, "no API key: pass one to zipbox.New or set ZIPBOX_API_KEY")
	}
	req := &request{method: method, url: c.baseURL + path, retry: retry, unwrap: unwrap, headers: map[string]string{
		"Authorization": "Bearer " + c.apiKey,
	}}
	for k, v := range headers {
		req.headers[k] = v
	}
	if in != nil {
		body, err := jsonBody(in)
		if err != nil {
			return err
		}
		req.body = body
		req.contentType = "application/json"
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return decodeJSON(resp.Body, out)
}

// decodeJSON reads one JSON body into out. A connection the server closed mid-body is
// the usual retryable network error, for the control plane and a box alike.
func decodeJSON(body io.Reader, out any) error {
	if err := json.NewDecoder(body).Decode(out); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return newError(CodeUnavailable, err.Error())
		}
		return err
	}
	return nil
}

// backoff is exponential with jitter: a delay in [d/2, d) where d doubles from
// the base up to the cap.
func (c *Client) backoff(attempt int) time.Duration {
	return jitter(c.ladder(attempt, c.backoffBase, c.backoffMax, 2), c)
}

// pollFactor is how much each re-read of a readiness wait grows. A box turns ready
// about a second after it serves, and nothing tells the client when, so each rung
// is time spent asleep after it did. Doubling overshot by up to several seconds;
// 1.25 keeps the same base, cap and jitter and reaches the cap gently.
const pollFactor = 1.25

// poll is the delay before a wait's next re-read: a delay in [d/2, d) where d
// grows by pollFactor from the poll interval up to its cap.
func (c *Client) poll(attempt int) time.Duration {
	return jitter(c.ladder(attempt, c.pollMin, c.pollMax, pollFactor), c)
}

func (c *Client) ladder(attempt int, base, max time.Duration, factor float64) time.Duration {
	d := base
	for i := 0; i < attempt && d < max; i++ {
		d = time.Duration(float64(d) * factor)
	}
	if d > max {
		d = max
	}
	return d
}

func jitter(d time.Duration, c *Client) time.Duration {
	if d <= 1 {
		return d
	}
	c.mu.Lock()
	n := c.rnd.Int63n(int64(d / 2))
	c.mu.Unlock()
	return d/2 + time.Duration(n)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newID() string {
	var b [16]byte
	cryptoRead(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out)
}
