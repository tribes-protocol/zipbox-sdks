package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ptySubprotocol is the marker the box expects before the token in
// Sec-WebSocket-Protocol.
const ptySubprotocol = "tribes.session"

// Box close codes on the terminal socket.
const (
	ptyCloseRevoked        = 4401
	ptyCloseUnknownSession = 4404
)

// ErrPtyTaken is returned by Pty.Read when another client took the session over.
var ErrPtyTaken = errors.New("zipbox: the terminal session was taken over by another client")

// ErrPtyClosed is returned by Pty methods after Close or Detach.
var ErrPtyClosed = errors.New("zipbox: the terminal is closed")

// PtyService opens terminals on a box. Reach it as Box.Pty.
type PtyService struct {
	box *Box
}

// PtyOptions configures a terminal.
type PtyOptions struct {
	// Cols and Rows are the initial size. Zero leaves the box's default.
	Cols, Rows int
	// ClientLabel names this client in the box's session list. Default "zipbox-sdk".
	ClientLabel string
}

// Create opens a new shell session (bash) and attaches to it. The session
// outlives the connection: Detach leaves it running and Attach re-joins it.
func (s *PtyService) Create(ctx context.Context, opts PtyOptions) (*Pty, error) {
	return s.open(ctx, "bash-"+newID(), opts, false)
}

// Attach re-joins an existing session by id; the box replays its screen.
func (s *PtyService) Attach(ctx context.Context, sessionID string, opts PtyOptions) (*Pty, error) {
	return s.open(ctx, sessionID, opts, true)
}

func (s *PtyService) open(ctx context.Context, sessionID string, opts PtyOptions, resume bool) (*Pty, error) {
	if opts.ClientLabel == "" {
		opts.ClientLabel = "zipbox-sdk"
	}
	runCtx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	p := &Pty{
		SessionID: sessionID,
		ClientID:  strings.ReplaceAll(newID(), "-", ""),
		box:       s.box,
		label:     opts.ClientLabel,
		cols:      opts.Cols,
		rows:      opts.Rows,
		out:       reader,
		outW:      writer,
		ctx:       runCtx,
		cancel:    cancel,
		connected: make(chan struct{}),
		done:      make(chan struct{}),
	}
	conn, err := p.dial(ctx, resume)
	if err != nil {
		cancel()
		return nil, err
	}
	p.setConn(conn)
	go p.run(conn)
	return p, nil
}

// Pty is an attached terminal. Read returns the session's output (io.EOF once
// the shell exits); Write sends keystrokes. A dropped connection reconnects by
// itself with exponential backoff and no attempt limit; Write waits for it.
type Pty struct {
	// SessionID names the session on the box (bash-<uuid>).
	SessionID string
	// ClientID identifies this client to the box; it is reused on reconnect.
	ClientID string

	box    *Box
	label  string
	out    *io.PipeReader
	outW   *io.PipeWriter
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	conn      *websocket.Conn
	connected chan struct{}
	cols      int
	rows      int
	exitCode  *int
	finalErr  error
	closed    bool
	done      chan struct{}
}

func (p *Pty) socketURL(baseURL string, resume bool) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	// The terminal is the root of the box's base URL, which may carry a path (a
	// local box answers under one).
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	q := url.Values{"session": {p.SessionID}, "clientId": {p.ClientID}, "clientLabel": {p.label}}
	if resume {
		q.Set("resume", "1")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one socket. A 401 re-fetches the box token once.
func (p *Pty) dial(ctx context.Context, resume bool) (*websocket.Conn, error) {
	for attempt := 0; ; attempt++ {
		creds, err := p.box.client.boxCreds(ctx, p.box.ID, attempt > 0)
		if err != nil {
			return nil, err
		}
		target, err := p.socketURL(creds.baseURL, resume)
		if err != nil {
			return nil, err
		}
		conn, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
			Subprotocols: []string{ptySubprotocol, creds.token},
			HTTPClient:   p.box.client.httpClient,
			HTTPHeader:   http.Header{"User-Agent": {"zipbox-sdk-go/" + Version}},
		})
		if err == nil {
			conn.SetReadLimit(64 << 20)
			return conn, nil
		}
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if resp != nil && resp.StatusCode >= 400 {
			return nil, statusError(resp, "terminal connection refused", 0, false)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newError(CodeUnavailable, "terminal connection failed: "+err.Error())
	}
}

func (p *Pty) setConn(conn *websocket.Conn) {
	p.mu.Lock()
	p.conn = conn
	close(p.connected)
	cols, rows := p.cols, p.rows
	p.mu.Unlock()
	if cols > 0 && rows > 0 {
		_ = p.send(conn, map[string]any{"t": "r", "c": cols, "r": rows})
	}
}

// markDead retires a socket exactly once, whoever notices first (the reader or
// a writer), so writers wait on the channel the next setConn closes.
func (p *Pty) markDead(conn *websocket.Conn) {
	p.mu.Lock()
	if p.conn == conn {
		p.conn = nil
		p.connected = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *Pty) send(conn *websocket.Conn, frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.Write(p.ctx, websocket.MessageText, data)
}

type ptyServerFrame struct {
	T    string `json:"t"`
	D    string `json:"d"`
	Code *int   `json:"code"`
}

// run reads frames until the session ends or the Pty is closed, reconnecting
// a dropped socket. The backoff ladder resets only when a frame arrives — a
// socket that connects and drops at once keeps climbing it.
func (p *Pty) run(conn *websocket.Conn) {
	defer close(p.done)
	failures := 0
	refreshed := false
	go p.keepalive()
	for {
		err := p.readLoop(conn, &failures)
		if p.ctx.Err() != nil {
			return
		}
		if failures == 0 {
			// A frame arrived on this socket: a later revoke gets its own re-fetch.
			refreshed = false
		}
		var fatal error
		switch websocket.CloseStatus(err) {
		case ptyCloseUnknownSession:
			fatal = newError(CodeNotFound, "the terminal session no longer exists")
		case ptyCloseRevoked:
			if refreshed {
				fatal = newError(CodeUnauthorized, "the box revoked this client's access")
			}
			refreshed = true
			p.box.client.forget(p.box.ID)
		}
		if errors.Is(err, errSessionEnded) || errors.Is(err, ErrPtyTaken) {
			fatal = err
		}
		if fatal != nil {
			p.finish(fatal)
			return
		}
		p.markDead(conn)
		for {
			if err := sleep(p.ctx, p.box.client.backoff(failures)); err != nil {
				return
			}
			failures++
			next, err := p.dial(p.ctx, true)
			if err == nil {
				conn = next
				p.setConn(conn)
				break
			}
			if p.ctx.Err() != nil {
				return
			}
			if zerr, ok := asError(err); ok && (zerr.Code == CodeUnauthorized || zerr.Code == CodeNotFound || zerr.Code == CodeForbidden) {
				p.finish(err)
				return
			}
		}
	}
}

var errSessionEnded = errors.New("session ended")

func (p *Pty) readLoop(conn *websocket.Conn, failures *int) error {
	for {
		_, data, err := conn.Read(p.ctx)
		if err != nil {
			return err
		}
		*failures = 0
		var frame ptyServerFrame
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.T {
		case "o":
			if _, err := p.outW.Write([]byte(frame.D)); err != nil {
				return errSessionEnded
			}
		case "x":
			p.mu.Lock()
			p.exitCode = frame.Code
			p.mu.Unlock()
			conn.Close(websocket.StatusNormalClosure, "exited")
			return errSessionEnded
		case "blocked":
			_ = p.send(conn, map[string]string{"t": "takeover"})
		case "owned":
			// A socket that was blocked dropped its size frame; re-send it now
			// that the box takes input from this socket.
			p.mu.Lock()
			cols, rows := p.cols, p.rows
			p.mu.Unlock()
			if cols > 0 && rows > 0 {
				_ = p.send(conn, map[string]any{"t": "r", "c": cols, "r": rows})
			}
		case "taken":
			conn.Close(websocket.StatusNormalClosure, "taken")
			return ErrPtyTaken
		}
	}
}

func (p *Pty) keepalive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	n := 0
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			conn := p.conn
			p.mu.Unlock()
			if conn != nil {
				n++
				_ = p.send(conn, map[string]any{"t": "ping", "n": n})
			}
		}
	}
}

func (p *Pty) finish(err error) {
	p.mu.Lock()
	if p.finalErr == nil {
		p.finalErr = err
	}
	p.mu.Unlock()
	if errors.Is(err, errSessionEnded) {
		p.outW.Close()
		return
	}
	p.outW.CloseWithError(err)
}

// Read reads terminal output. It returns io.EOF after the shell exits.
func (p *Pty) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write sends input. While the socket is reconnecting it waits.
func (p *Pty) Write(b []byte) (int, error) {
	if err := p.sendFrame(map[string]string{"t": "i", "d": string(b)}); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize sets the terminal size; it is re-applied after every reconnect.
func (p *Pty) Resize(cols, rows int) error {
	p.mu.Lock()
	p.cols, p.rows = cols, rows
	p.mu.Unlock()
	return p.sendFrame(map[string]any{"t": "r", "c": cols, "r": rows})
}

func (p *Pty) sendFrame(frame any) error {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrPtyClosed
		}
		if p.finalErr != nil {
			err := p.finalErr
			p.mu.Unlock()
			if errors.Is(err, errSessionEnded) {
				return io.EOF
			}
			return err
		}
		conn, ready := p.conn, p.connected
		p.mu.Unlock()
		if conn != nil {
			if err := p.send(conn, frame); err == nil {
				return nil
			} else if p.ctx.Err() != nil {
				return ErrPtyClosed
			}
			// The socket died under the write; wait for the reconnect.
			p.markDead(conn)
			continue
		}
		select {
		case <-ready:
		case <-p.done:
		case <-p.ctx.Done():
			return ErrPtyClosed
		}
	}
}

// ExitCode is the shell's exit status once it has exited, else nil.
func (p *Pty) ExitCode() *int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// Detach closes this connection and leaves the session running on the box.
func (p *Pty) Detach() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	conn := p.conn
	p.mu.Unlock()
	p.cancel()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "detach")
	}
	p.outW.CloseWithError(ErrPtyClosed)
	<-p.done
	return nil
}

// Close kills the session on the box and closes the connection.
func (p *Pty) Close() error {
	var out struct {
		Closed bool `json:"closed"`
	}
	query := url.Values{"session": {p.SessionID}, "clientId": {p.ClientID}}
	err := p.box.json(context.Background(), "POST", "/session/close", query, nil, &out)
	_ = p.Detach()
	return err
}
