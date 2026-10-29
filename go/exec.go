package zipbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ExecOption configures a command.
type ExecOption func(*execRequest)

// WithCwd sets the working directory: absolute, or relative to /root/workspace
// (the default).
func WithCwd(dir string) ExecOption { return func(r *execRequest) { r.Cwd = dir } }

// WithEnv adds environment variables on top of the box's own.
func WithEnv(env map[string]string) ExecOption {
	return func(r *execRequest) {
		if r.Env == nil {
			r.Env = map[string]string{}
		}
		for k, v := range env {
			r.Env[k] = v
		}
	}
}

// WithUser runs the command as another local user. Default: the box user (root).
func WithUser(user string) ExecOption { return func(r *execRequest) { r.User = user } }

// WithTimeout asks the box to kill the command after d. Without it a command
// runs until it exits or is signalled.
func WithTimeout(d time.Duration) ExecOption {
	return func(r *execRequest) { r.TimeoutMs = d.Milliseconds() }
}

// WithStdin opens a stdin pipe fed by Command.Stdin. Default: /dev/null.
func WithStdin() ExecOption { return func(r *execRequest) { r.Stdin = true } }

type execRequest struct {
	Argv       []string          `json:"argv,omitempty"`
	Cmd        string            `json:"cmd,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	TimeoutMs  int64             `json:"timeoutMs,omitempty"`
	Background bool              `json:"background,omitempty"`
	Stdin      bool              `json:"stdin,omitempty"`
}

// ExecResult is a finished command. A non-zero ExitCode is a result, not an error.
type ExecResult struct {
	// ID is the command id on the box.
	ID string `json:"id"`
	// ExitCode is shell-style: 0..255, 128+N when killed by signal N, -1 when
	// the box lost track of the process (state "unknown").
	ExitCode int `json:"exitCode"`
	// Stdout and Stderr are the command's output, decoded as UTF-8.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Signal is the signal the box delivered (a Signal call or a timeout), if any.
	Signal *string `json:"signal,omitempty"`
	// TimedOut is true when WithTimeout ended the command.
	TimedOut bool `json:"timedOut"`
}

// ExecEventType is the kind of an ExecEvent.
type ExecEventType string

// Event types on a command's stream.
const (
	EventStart  ExecEventType = "start"
	EventStdout ExecEventType = "stdout"
	EventStderr ExecEventType = "stderr"
	EventGap    ExecEventType = "gap"
	EventExit   ExecEventType = "exit"
)

// ExecEvent is one event on a command's stream. Which fields are set depends
// on Type.
type ExecEvent struct {
	Type ExecEventType `json:"type"`
	// Cursor resumes the stream after this event (Commands.Events since).
	Cursor string `json:"cursor"`
	// ID is set on start.
	ID string `json:"id,omitempty"`
	// Data is set on stdout and stderr.
	Data string `json:"data,omitempty"`
	// Stream and Message are set on gap: output past the 64 MiB per-stream cap
	// was discarded (the process keeps running).
	Stream  string `json:"stream,omitempty"`
	Message string `json:"message,omitempty"`
	// State, ExitCode, Signal and TimedOut are set on exit.
	State    string  `json:"state,omitempty"`
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
	TimedOut bool    `json:"timedOut,omitempty"`
}

// ExecStream reads a command's events. Use it like bufio.Scanner and always
// Close it:
//
//	for stream.Next() { ev := stream.Event() … }
//	if err := stream.Err(); err != nil { … }
//
// A stream that drops once its command has started re-attaches from the last
// cursor by itself: a followed command is a live surface, so there is no attempt
// ceiling, the backoff is capped and jittered (WithBackoff), and the ladder
// resets only when an event arrives. Only an answered error, ctx or Close ends it.
type ExecStream struct {
	ctx    context.Context
	box    *Box
	sse    *sseReader
	ev     ExecEvent
	err    error
	done   bool
	cursor string
	// mu guards body and closed: Close may run while Next is blocked in a read.
	mu     sync.Mutex
	body   io.Closer
	closed bool
	// commandID is the command the stream follows; empty until a foreground
	// command's start event, and a stream without one cannot re-attach.
	commandID string
	failures  int
}

func newExecStream(ctx context.Context, box *Box, body io.ReadCloser, cursor, commandID string) *ExecStream {
	s := &ExecStream{ctx: ctx, box: box, cursor: cursor, commandID: commandID}
	s.attach(body)
	return s
}

// attach makes body the current connection. A body handed to a stream that was
// closed meanwhile is closed at once and not attached.
func (s *ExecStream) attach(body io.ReadCloser) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.sse = nil, nil
	if body == nil {
		return true
	}
	if s.closed {
		body.Close()
		return false
	}
	s.body, s.sse = body, newSSEReader(body)
	return true
}

func (s *ExecStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// reattach re-opens the command's events from the cursor, with backoff, until
// one opens or an answered error ends the stream. A Close ends it too.
func (s *ExecStream) reattach() bool {
	s.mu.Lock()
	if s.body != nil {
		s.body.Close()
	}
	s.body, s.sse = nil, nil
	s.mu.Unlock()
	for {
		if s.isClosed() {
			s.err = errStreamClosed
			return false
		}
		if err := sleep(s.ctx, s.box.client.backoff(s.failures)); err != nil {
			s.err = err
			return false
		}
		s.failures++
		body, err := s.box.Commands.open(s.ctx, s.commandID, s.cursor)
		if err == nil {
			if !s.attach(body) {
				s.err = errStreamClosed
				return false
			}
			return true
		}
		if !retryable(err) {
			s.err = err
			return false
		}
	}
}

var errStreamClosed = newError(CodeUnavailable, "the event stream was closed before the command exited")

// Next advances to the next event. It returns false after the exit event, or
// on an error that ends the stream (see Err).
func (s *ExecStream) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	for {
		if s.sse == nil {
			if !s.reattach() {
				return false
			}
		}
		raw, err := s.sse.next()
		if err != nil {
			if s.isClosed() {
				s.err = errStreamClosed
				return false
			}
			if s.commandID != "" && s.ctx.Err() == nil {
				// A drop after the command started: resume from the cursor.
				s.sse = nil
				continue
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				s.err = newError(CodeUnavailable, "the event stream ended before the command exited")
			} else {
				s.err = err
			}
			return false
		}
		ev := ExecEvent{Type: ExecEventType(raw.event), Cursor: raw.id}
		if ev.Cursor == "" {
			ev.Cursor = s.cursor
		}
		switch ev.Type {
		case EventStart, EventStdout, EventStderr, EventGap, EventExit:
		default:
			continue
		}
		if raw.data != "" {
			if err := json.Unmarshal([]byte(raw.data), &ev); err != nil {
				s.err = err
				return false
			}
			ev.Type = ExecEventType(raw.event)
			if raw.id != "" {
				ev.Cursor = raw.id
			}
		}
		s.cursor = ev.Cursor
		s.ev = ev
		s.failures = 0
		if ev.Type == EventStart && s.commandID == "" {
			s.commandID = ev.ID
		}
		if ev.Type == EventExit {
			s.done = true
		}
		return true
	}
}

func retryable(err error) bool {
	var zerr *Error
	return errors.As(err, &zerr) && zerr.Retryable
}

// Event returns the current event.
func (s *ExecStream) Event() ExecEvent { return s.ev }

// Err returns the error that stopped the stream, or nil after a clean exit event.
func (s *ExecStream) Err() error { return s.err }

// Cursor is the resume cursor after the last event read.
func (s *ExecStream) Cursor() string { return s.cursor }

// Close releases the stream and stops it re-attaching; a Next in flight returns
// false. Closing the stream of a foreground Exec before it exits makes the box
// kill the command.
func (s *ExecStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.body == nil {
		return nil
	}
	err := s.body.Close()
	s.body = nil
	return err
}

// collect drains a stream into a result. It stops at the exit event.
type collector struct {
	result ExecResult
	stdout strings.Builder
	stderr strings.Builder
	exited bool
}

func (c *collector) add(ev ExecEvent) {
	switch ev.Type {
	case EventStart:
		c.result.ID = ev.ID
	case EventStdout:
		c.stdout.WriteString(ev.Data)
	case EventStderr:
		c.stderr.WriteString(ev.Data)
	case EventExit:
		c.exited = true
		c.result.ExitCode = -1
		if ev.ExitCode != nil {
			c.result.ExitCode = *ev.ExitCode
		}
		c.result.Signal = ev.Signal
		c.result.TimedOut = ev.TimedOut
	}
}

func (c *collector) done() *ExecResult {
	c.result.Stdout = c.stdout.String()
	c.result.Stderr = c.stderr.String()
	return &c.result
}

// Exec runs cmd through `bash -lc` (so PATH, nvm, pyenv and the harness env are
// present) and waits for it to exit.
func (b *Box) Exec(ctx context.Context, cmd string, opts ...ExecOption) (*ExecResult, error) {
	return b.runToResult(ctx, &execRequest{Cmd: cmd}, opts)
}

// ExecArgv spawns argv directly, without a shell, and waits for it to exit.
func (b *Box) ExecArgv(ctx context.Context, argv []string, opts ...ExecOption) (*ExecResult, error) {
	return b.runToResult(ctx, &execRequest{Argv: argv}, opts)
}

func (b *Box) runToResult(ctx context.Context, req *execRequest, opts []ExecOption) (*ExecResult, error) {
	stream, err := b.stream(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var c collector
	for stream.Next() {
		c.add(stream.Event())
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return c.done(), nil
}

// ExecStream runs cmd through `bash -lc` and streams its events. The command
// is killed if the stream is closed before it exits; use ExecBackground for a
// command that must outlive the caller.
func (b *Box) ExecStream(ctx context.Context, cmd string, opts ...ExecOption) (*ExecStream, error) {
	return b.stream(ctx, &execRequest{Cmd: cmd}, opts)
}

// ExecStreamArgv is ExecStream for argv spawned directly, without a shell.
func (b *Box) ExecStreamArgv(ctx context.Context, argv []string, opts ...ExecOption) (*ExecStream, error) {
	return b.stream(ctx, &execRequest{Argv: argv}, opts)
}

func (b *Box) stream(ctx context.Context, req *execRequest, opts []ExecOption) (*ExecStream, error) {
	for _, opt := range opts {
		opt(req)
	}
	body, err := jsonBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(ctx, &request{
		method:      "POST",
		body:        body,
		contentType: "application/json",
		headers:     map[string]string{"Accept": "text/event-stream"},
	}, "/exec")
	if err != nil {
		return nil, err
	}
	return newExecStream(ctx, b, resp.Body, "0-0", ""), nil
}

// ExecBackground starts cmd through `bash -lc` and returns as soon as it is
// running. The command keeps running whether or not anyone is attached, and
// survives a restart of the box's agent. Follow it with Command.Wait or
// Command.Events.
func (b *Box) ExecBackground(ctx context.Context, cmd string, opts ...ExecOption) (*Command, error) {
	return b.background(ctx, &execRequest{Cmd: cmd, Background: true}, opts)
}

// ExecBackgroundArgv is ExecBackground for argv spawned directly, without a shell.
func (b *Box) ExecBackgroundArgv(ctx context.Context, argv []string, opts ...ExecOption) (*Command, error) {
	return b.background(ctx, &execRequest{Argv: argv, Background: true}, opts)
}

func (b *Box) background(ctx context.Context, req *execRequest, opts []ExecOption) (*Command, error) {
	for _, opt := range opts {
		opt(req)
	}
	var record CommandRecord
	if err := b.json(ctx, "POST", "/exec", nil, req, &record); err != nil {
		return nil, err
	}
	return &Command{CommandRecord: record, box: b}, nil
}

// Language is a RunCode language.
type Language string

// RunCode languages.
const (
	Python Language = "python"
	Node   Language = "node"
	Bash   Language = "bash"
)

// RunCode writes code to a temporary file on the box, runs it with python3, node
// (as an ES module) or bash through the login shell, then removes it. It is
// sugar over Files.Write and Exec.
func (b *Box) RunCode(ctx context.Context, lang Language, code string, opts ...ExecOption) (*ExecResult, error) {
	var interpreter, ext string
	switch lang {
	case Python:
		interpreter, ext = "python3", ".py"
	case Node:
		interpreter, ext = "node", ".mjs"
	case Bash:
		interpreter, ext = "bash", ".sh"
	default:
		return nil, newError(CodeInvalidRequest, "unsupported language "+string(lang)+" (python, node, bash)")
	}
	file := "/tmp/zipbox-run-" + newID() + ext
	if err := b.Files.Write(ctx, file, strings.NewReader(code)); err != nil {
		return nil, err
	}
	script := interpreter + " '" + file + "'; status=$?; rm -f '" + file + "'; exit $status"
	return b.Exec(ctx, script, opts...)
}

// Signal names a command can be sent.
type Signal string

// Signals.
const (
	SIGTERM Signal = "TERM"
	SIGKILL Signal = "KILL"
	SIGINT  Signal = "INT"
	SIGHUP  Signal = "HUP"
	SIGQUIT Signal = "QUIT"
)

// CommandRecord is a command as the box reports it.
type CommandRecord struct {
	ID         string   `json:"id"`
	State      string   `json:"state"`
	Argv       []string `json:"argv"`
	Cwd        string   `json:"cwd"`
	Background bool     `json:"background"`
	// ExitCode is nil while the command runs.
	ExitCode    *int       `json:"exitCode,omitempty"`
	Signal      *string    `json:"signal,omitempty"`
	TimedOut    bool       `json:"timedOut"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	StdoutBytes int64      `json:"stdoutBytes"`
	StderrBytes int64      `json:"stderrBytes"`
}

// Command is a handle to a command on a box.
type Command struct {
	CommandRecord
	box *Box
}

// Command returns a handle to an existing command by id.
func (b *Box) Command(id string) *Command {
	return &Command{CommandRecord: CommandRecord{ID: id}, box: b}
}

// Get re-reads the command's record.
func (c *Command) Get(ctx context.Context) (*CommandRecord, error) {
	record, err := c.box.Commands.Get(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	c.CommandRecord = *record
	return record, nil
}

// Events streams the command's events after since ("" = from the start).
func (c *Command) Events(ctx context.Context, since string) (*ExecStream, error) {
	return c.box.Commands.Events(ctx, c.ID, since)
}

// Stdin appends r's bytes to the command's stdin (start it WithStdin).
func (c *Command) Stdin(ctx context.Context, r io.Reader) error {
	return c.box.Commands.Stdin(ctx, c.ID, r)
}

// CloseStdin closes the command's stdin.
func (c *Command) CloseStdin(ctx context.Context) error {
	return c.box.Commands.CloseStdin(ctx, c.ID)
}

// Signal sends sig to the command's process group.
func (c *Command) Signal(ctx context.Context, sig Signal) error {
	return c.box.Commands.Signal(ctx, c.ID, sig)
}

// Wait follows the command until it exits and returns everything it printed.
// A dropped stream is resumed from its cursor with backoff, without limit;
// only an answered error (not_found, unauthorized, …) or ctx ends the wait.
func (c *Command) Wait(ctx context.Context) (*ExecResult, error) {
	stream, err := c.box.Commands.Events(ctx, c.ID, "")
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var col collector
	for stream.Next() {
		col.add(stream.Event())
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	res := col.done()
	res.ID = c.ID
	return res, nil
}

// CommandsService manages a box's commands. Reach it as Box.Commands.
type CommandsService struct {
	box *Box
}

// Get returns one command's record.
func (s *CommandsService) Get(ctx context.Context, id string) (*CommandRecord, error) {
	var out CommandRecord
	if err := s.box.json(ctx, "GET", "/exec/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns running and recently finished commands.
func (s *CommandsService) List(ctx context.Context) ([]CommandRecord, error) {
	var out struct {
		Commands []CommandRecord `json:"commands"`
	}
	if err := s.box.json(ctx, "GET", "/exec", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Commands, nil
}

// Events replays a command's events after since ("" = from the start) and
// follows it until it exits, re-attaching from the last cursor whenever the
// stream drops (see ExecStream). An answered error on the first attach is
// returned here.
func (s *CommandsService) Events(ctx context.Context, id, since string) (*ExecStream, error) {
	cursor := since
	if cursor == "" {
		cursor = "0-0"
	}
	body, err := s.open(ctx, id, since)
	if err != nil && !retryable(err) {
		return nil, err
	}
	return newExecStream(ctx, s.box, body, cursor, id), nil
}

// open is one connection to a command's events after since.
func (s *CommandsService) open(ctx context.Context, id, since string) (io.ReadCloser, error) {
	query := url.Values{}
	if since != "" {
		query.Set("since", since)
	}
	resp, err := s.box.do(ctx, &request{
		method:  "GET",
		query:   query,
		retry:   true,
		headers: map[string]string{"Accept": "text/event-stream"},
	}, "/exec/"+url.PathEscape(id)+"/events")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Stdin appends r's bytes to a command's stdin.
func (s *CommandsService) Stdin(ctx context.Context, id string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return s.raw(ctx, "/exec/"+url.PathEscape(id)+"/stdin", nil, data)
}

// CloseStdin closes a command's stdin.
func (s *CommandsService) CloseStdin(ctx context.Context, id string) error {
	return s.raw(ctx, "/exec/"+url.PathEscape(id)+"/stdin", url.Values{"close": {"1"}}, nil)
}

func (s *CommandsService) raw(ctx context.Context, boxPath string, query url.Values, data []byte) error {
	resp, err := s.box.do(ctx, &request{
		method:      "POST",
		query:       query,
		body:        func() (io.Reader, error) { return bytes.NewReader(data), nil },
		contentType: "application/octet-stream",
	}, boxPath)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Signal sends sig to a command's process group.
func (s *CommandsService) Signal(ctx context.Context, id string, sig Signal) error {
	return s.box.json(ctx, "POST", "/exec/"+url.PathEscape(id)+"/signal", nil, map[string]Signal{"signal": sig}, nil)
}
