package zipbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
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

	client *Client
}

// Sandbox returns a handle to the box with this id. It makes no network call.
func (c *Client) Sandbox(id string) *Box {
	b := &Box{ID: id, client: c}
	b.Files = &FilesService{box: b}
	b.Commands = &CommandsService{box: b}
	b.Pty = &PtyService{box: b}
	b.Preview = &PreviewService{box: b}
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

type boxCreds struct {
	baseURL string
	token   string
	detail  *SandboxDetail
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
			return cached, nil
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
	defer c.mu.Unlock()
	return c.creds[id], nil
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

// host is the hostname the box serves on (its baseURL's host, without port).
func (b *Box) host(ctx context.Context) (string, error) {
	creds, err := b.client.boxCreds(ctx, b.ID, false)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(creds.baseURL)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
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
