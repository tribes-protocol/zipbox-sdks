// Package zipbox is the Go SDK for Zipbox: create boxes, run commands, move
// files, open terminals and publish ports from code.
//
//	c := zipbox.New("") // reads ZIPBOX_API_KEY
//	box, err := c.Sandboxes.GetOrCreate(ctx, "demo")
//	res, err := box.Exec(ctx, "echo hello")
//	fmt.Print(res.Stdout)
//
// Nothing on a box stops on a clock, and no call has a default deadline: pass a
// context with a deadline when you want one.
package zipbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Version is the SDK version, sent in the User-Agent header.
const Version = "0.1.0"

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

	mu    sync.Mutex
	creds map[string]*boxCreds
	rnd   *rand.Rand
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

// WithBackoff sets the exponential backoff used between retries and PTY
// reconnects: the first delay and the cap. Default 500ms and 30s.
func WithBackoff(base, max time.Duration) Option {
	return func(c *Client) { c.backoffBase, c.backoffMax = base, max }
}

// WithPollInterval sets how often Create re-reads a box while it boots: the
// first delay and the cap. Default 1s and 5s.
func WithPollInterval(min, max time.Duration) Option {
	return func(c *Client) { c.pollMin, c.pollMax = min, max }
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
		pollMax:     5 * time.Second,
		creds:       map[string]*boxCreds{},
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())),
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
func (c *Client) do(ctx context.Context, req *request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, req)
		if err == nil {
			return resp, nil
		}
		zerr, ok := asError(err)
		if !req.retry || !ok || attempt >= c.maxRetries ||
			(zerr.Code != CodeRateLimited && zerr.Code != CodeUnavailable) {
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
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, body)
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
		return nil, zerr
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return nil, errorFromResponse(resp, data)
}

// api calls the control plane with the account key.
func (c *Client) api(ctx context.Context, method, path string, in any, out any, retry bool, headers map[string]string) error {
	if c.apiKey == "" {
		return newError(CodeUnauthorized, "no API key: pass one to zipbox.New or set ZIPBOX_API_KEY")
	}
	req := &request{method: method, url: c.baseURL + path, retry: retry, headers: map[string]string{
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
	return json.NewDecoder(resp.Body).Decode(out)
}

// backoff is exponential with jitter: a delay in [d/2, d) where d doubles from
// the base up to the cap.
func (c *Client) backoff(attempt int) time.Duration {
	return jitter(c.ladder(attempt, c.backoffBase, c.backoffMax), c)
}

func (c *Client) ladder(attempt int, base, max time.Duration) time.Duration {
	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
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
