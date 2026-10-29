package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Box is a handle to one box. It is cheap to create (Client.Sandbox makes no
// call) and safe for concurrent use. The box token it needs is fetched on first
// use and cached by the client.
type Box struct {
	// ID is the box id.
	ID string
	// Detail is the last box row this handle read, when it read one.
	Detail *SandboxDetail

	// Files reads and writes the box's filesystem.
	Files *FilesService
	// Commands manages commands started with ExecBackground.
	Commands *CommandsService
	// Pty opens interactive terminals on the box.
	Pty *PtyService
	// Preview publishes a local port at a public HTTPS URL.
	Preview *PreviewService
	// Tailscale joins the box to your own tailnet.
	Tailscale *TailscaleService

	client *Client
}

// Sandbox returns a handle to the box with this id. It makes no network call.
func (c *Client) Sandbox(id string) *Box {
	b := &Box{ID: id, client: c}
	b.Files = &FilesService{box: b}
	b.Commands = &CommandsService{box: b}
	b.Pty = &PtyService{box: b}
	b.Preview = &PreviewService{box: b}
	b.Tailscale = &TailscaleService{box: b}
	return b
}

// Refresh re-reads the box row.
func (b *Box) Refresh(ctx context.Context) (*SandboxDetail, error) {
	detail, err := b.client.Sandboxes.Get(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	b.Detail = detail
	if detail.Ready {
		b.client.storeCreds(b.ID, detail)
	}
	return detail, nil
}

// WaitReady re-reads the box with backoff until it is ready (see
// SandboxesService.WaitReady).
func (b *Box) WaitReady(ctx context.Context) (*SandboxDetail, error) {
	ready, err := b.client.Sandboxes.WaitReady(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	b.Detail = ready.Detail
	return b.Detail, nil
}

// Start starts the box if it is stopped (see SandboxesService.Start).
func (b *Box) Start(ctx context.Context) error { return b.client.Sandboxes.Start(ctx, b.ID) }

// Stop stops the box: its disk is archived and nothing is billed while stopped.
func (b *Box) Stop(ctx context.Context) error { return b.client.Sandboxes.Stop(ctx, b.ID) }

// Restart hard-restarts the box. Files survive; running processes do not.
func (b *Box) Restart(ctx context.Context) error { return b.client.Sandboxes.Restart(ctx, b.ID) }

// Delete deletes the box (see SandboxesService.Delete).
func (b *Box) Delete(ctx context.Context, opts DeleteOptions) error {
	return b.client.Sandboxes.Delete(ctx, b.ID, opts)
}

// SetTier moves the box to another tier. A running box restarts into it.
func (b *Box) SetTier(ctx context.Context, tier string) error {
	return b.client.Sandboxes.SetTier(ctx, b.ID, tier)
}

type boxCreds struct {
	baseURL string
	token   string
	detail  *SandboxDetail
	// reachable records a private box that answered the reachability probe.
	reachable bool
}

func (c *Client) storeCreds(id string, detail *SandboxDetail) {
	if !detail.Ready || detail.BaseURL == nil || detail.BoxToken == nil {
		return
	}
	c.mu.Lock()
	c.creds[id] = &boxCreds{baseURL: strings.TrimRight(*detail.BaseURL, "/"), token: *detail.BoxToken, detail: detail}
	c.mu.Unlock()
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.creds, id)
	c.mu.Unlock()
}

// boxCreds returns the cached origin and token for a box, fetching them when
// absent or when refresh is set. A box that is not ready is box_not_running.
func (c *Client) boxCreds(ctx context.Context, id string, refresh bool) (*boxCreds, error) {
	if !refresh {
		c.mu.Lock()
		cached := c.creds[id]
		c.mu.Unlock()
		if cached != nil {
			return c.ensureReachable(ctx, id, cached)
		}
	}
	detail, err := c.Sandboxes.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !detail.Ready || detail.BaseURL == nil {
		c.forget(id)
		return nil, newError(CodeBoxNotRunning, "box "+id+" is "+string(detail.Status)+", not ready; start it or wait")
	}
	if detail.BoxToken == nil || *detail.BoxToken == "" {
		return nil, newError(CodeUnauthorized, "the control plane returned no box token; box APIs need an API key")
	}
	c.storeCreds(id, detail)
	c.mu.Lock()
	creds := c.creds[id]
	c.mu.Unlock()
	return c.ensureReachable(ctx, id, creds)
}

// defaultPrivateProbeTimeout bounds the reachability probe of a private box. It is a
// probe, not a call: the box-direct call itself still has no deadline.
const defaultPrivateProbeTimeout = 5 * time.Second

// unroutable reports a failure to reach the box's address at all: DNS, a failed
// dial, or the probe deadline. A TLS failure is not one: the box answered.
func unroutable(err error) bool {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &dnsErr) ||
		(errors.As(err, &opErr) && opErr.Op == "dial")
}

// ensureReachable checks, once per box, that a private box answers from this
// device. A private box answers only devices on its owner's tailnet; off the
// tailnet its address does not route and a call would hang, so a network
// failure is box_private_unreachable instead. Any HTTP answer counts as
// reachable, and only success is cached.
func (c *Client) ensureReachable(ctx context.Context, id string, creds *boxCreds) (*boxCreds, error) {
	if creds.detail == nil || !creds.detail.PrivateMode {
		return creds, nil
	}
	c.mu.Lock()
	reachable := creds.reachable
	c.mu.Unlock()
	if reachable {
		return creds, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.privateProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, "GET", creds.baseURL+"/challenge", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zipbox-sdk-go/"+Version)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if unroutable(err) {
			name := id
			if creds.detail.Name != nil && *creds.detail.Name != "" {
				name = *creds.detail.Name
			}
			return nil, newError(CodeBoxPrivateUnreachable, name+" is private: it can only be reached from devices on your tailnet. "+
				"Turn on Tailscale on this device (https://tailscale.com/download).")
		}
	} else {
		_ = resp.Body.Close()
	}
	c.mu.Lock()
	creds.reachable = true
	c.mu.Unlock()
	return creds, nil
}

// do sends a request to the box. A 401 re-fetches the box token once and
// retries; a second 401 is returned as unauthorized.
func (b *Box) do(ctx context.Context, req *request, boxPath string) (*boxResponse, error) {
	for attempt := 0; ; attempt++ {
		creds, err := b.client.boxCreds(ctx, b.ID, attempt > 0)
		if err != nil {
			return nil, err
		}
		r := *req
		r.url = creds.baseURL + boxPath
		r.headers = map[string]string{"Authorization": "Bearer " + creds.token}
		for k, v := range req.headers {
			r.headers[k] = v
		}
		resp, err := b.client.do(ctx, &r)
		if err == nil {
			return &boxResponse{Response: resp, creds: creds}, nil
		}
		if attempt == 0 && IsCode(err, CodeUnauthorized) {
			continue
		}
		if IsCode(err, CodeUnauthorized) {
			// Not a deleted key: that fails the re-read above, at the control plane.
			// The control plane just vouched for this key, so the box has not
			// received its token yet.
			zerr := newError(CodeUnauthorized, "box "+b.ID+" has not received this API key's token yet: "+
				"the control plane accepts the key, the box does not. A key created moments ago "+
				"reaches its running boxes within seconds; retry then")
			zerr.Status = http.StatusUnauthorized
			zerr.Retryable = true
			return nil, zerr
		}
		return nil, err
	}
}

func (b *Box) json(ctx context.Context, method, boxPath string, query url.Values, in any, out any) error {
	req := &request{method: method, query: query, retry: method == "GET"}
	if in != nil {
		body, err := jsonBody(in)
		if err != nil {
			return err
		}
		req.body = body
		req.contentType = "application/json"
	}
	resp, err := b.do(ctx, req, boxPath)
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

// host is the custom preview apex, without the platform transport prefix or port.
func (b *Box) host(ctx context.Context) (string, error) {
	creds, err := b.client.boxCreds(ctx, b.ID, false)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(creds.baseURL)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Hostname(), "box--"), nil
}

// WorkspaceDir is where relative paths resolve and commands start by default.
const WorkspaceDir = "/root/workspace"

// ResolvePath joins a relative path to WorkspaceDir; an absolute path is cleaned.
func ResolvePath(p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Join(WorkspaceDir, p)
}

type boxResponse struct {
	*http.Response
	creds *boxCreds
}
