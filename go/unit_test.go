package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSSEReader(t *testing.T) {
	stream := ": comment\r\nid: 1-0\r\nevent: stdout\r\ndata: {\"data\":\"a\"}\r\n\r\n" +
		"id: 1-2\nevent: stderr\ndata: line1\ndata: line2\n\n" +
		"event: exit\ndata: {}\n\n"
	r := newSSEReader(strings.NewReader(stream))
	want := []sseEvent{
		{id: "1-0", event: "stdout", data: `{"data":"a"}`},
		{id: "1-2", event: "stderr", data: "line1\nline2"},
		{event: "exit", data: "{}"},
	}
	for i, w := range want {
		got, err := r.next()
		if err != nil || got != w {
			t.Fatalf("event %d: got %+v, %v; want %+v", i, got, err, w)
		}
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("want clean EOF, got %v", err)
	}
	truncated := newSSEReader(strings.NewReader("id: 1-0\nevent: stdout\ndata: x"))
	if _, err := truncated.next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("want unexpected EOF mid-event, got %v", err)
	}
}

// A stream that ends before its command started has nothing to re-attach to.
func TestExecStreamEndsBeforeStartIsUnavailable(t *testing.T) {
	s := newExecStream(context.Background(), nil, io.NopCloser(strings.NewReader("")), "0-0", "")
	if s.Next() || !IsCode(s.Err(), CodeUnavailable) {
		t.Fatalf("err %v", s.Err())
	}
}

// A foreground command whose stream drops after start is followed from its cursor.
func TestExecResumesAfterADrop(t *testing.T) {
	var base string
	var resumed []string
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 0-0\nevent: start\ndata: {\"id\":\"cmd_1\"}\n\nid: 3-0\nevent: stdout\ndata: {\"data\":\"one\"}\n\n")
	})
	mux.HandleFunc("/exec/cmd_1/events", func(w http.ResponseWriter, r *http.Request) {
		resumed = append(resumed, r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 6-0\nevent: stdout\ndata: {\"data\":\"two\"}\n\nid: 6-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"timedOut\":false}\n\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
	res, err := c.Sandbox("sbx_1").Exec(context.Background(), "echo one; echo two")
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "cmd_1" || res.Stdout != "onetwo" || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if len(resumed) != 1 || resumed[0] != "since=3-0" {
		t.Fatalf("resume queries %v", resumed)
	}
}

func TestBackoffLadder(t *testing.T) {
	c := New("k", WithBackoff(500*time.Millisecond, 30*time.Second))
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := c.ladder(i, c.backoffBase, c.backoffMax, 2); got != w {
			t.Errorf("ladder(%d) = %v, want %v", i, got, w)
		}
		for j := 0; j < 50; j++ {
			if d := c.backoff(i); d < w/2 || d >= w {
				t.Fatalf("backoff(%d) = %v outside [%v, %v)", i, d, w/2, w)
			}
		}
	}
	if got := c.ladder(1000, c.backoffBase, c.backoffMax, 2); got != 30*time.Second {
		t.Fatalf("no ceiling on attempts, capped delay: got %v", got)
	}
}

// A box turns ready ~1 s after it serves; doubling slept up to seconds past it.
func TestPollLadder(t *testing.T) {
	c := New("k")
	want := []float64{1, 1.25, 1.5625, 1.953125, 2.44140625, 3.0517578125, 3.814697, 4.768372, 5.960464, 7.450581, 9.313226, 10, 10, 10}
	for i, w := range want {
		step := c.ladder(i, c.pollMin, c.pollMax, pollFactor)
		if diff := step.Seconds() - w; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("poll ladder(%d) = %v, want %vs", i, step, w)
		}
		for j := 0; j < 50; j++ {
			if d := c.poll(i); d < step/2 || d >= step {
				t.Fatalf("poll(%d) = %v outside [%v, %v)", i, d, step/2, step)
			}
		}
	}
	if got := c.ladder(1000, c.pollMin, c.pollMax, pollFactor); got != 10*time.Second {
		t.Fatalf("no ceiling on attempts, capped delay: got %v", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("2"); !ok || d != 2*time.Second {
		t.Fatalf("seconds: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("absent header must not count as a hint")
	}
	if d, ok := parseRetryAfter("0"); !ok || d != 0 {
		t.Fatalf("zero: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter(time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)); !ok {
		t.Fatal("http date")
	}
}

func TestResolvePath(t *testing.T) {
	cases := map[string]string{"a.txt": "/root/workspace/a.txt", "/etc/../etc/hosts": "/etc/hosts", "./x/../y": "/root/workspace/y"}
	for in, want := range cases {
		if got := ResolvePath(in); got != want {
			t.Errorf("ResolvePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLegacyBoxErrorMapsByStatus(t *testing.T) {
	resp := &http.Response{StatusCode: 413, Header: http.Header{}}
	zerr := errorFromResponse(resp, []byte(`{"error":"upload too large"}`))
	if zerr.Code != CodeInvalidRequest || zerr.Message != "upload too large" || zerr.DocsURL != "https://zipbox.ai/docs/errors#invalid_request" {
		t.Fatalf("%+v", zerr)
	}
}

func TestMachineLimitCodePassesThroughNotRetryable(t *testing.T) {
	resp := &http.Response{StatusCode: 403, Header: http.Header{}}
	zerr := errorFromResponse(resp, []byte(`{"error":{"code":"machine_limit_reached","message":"limit of 250 machines","requestId":"r1"}}`))
	if zerr.Code != CodeMachineLimitReached || zerr.Status != 403 || zerr.Retryable || zerr.Message != "limit of 250 machines" {
		t.Fatalf("%+v", zerr)
	}
	if zerr.DocsURL != "https://zipbox.ai/docs/errors#machine_limit_reached" {
		t.Fatalf("docs: %q", zerr.DocsURL)
	}
	if !IsCode(zerr, CodeMachineLimitReached) {
		t.Fatal("IsCode")
	}
}

// detailHandler answers GET /sandbox/sbx_1 with a ready box pointing at base.
func detailHandler(base *string, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"sbx_1","status":"running","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":%q}`, *base, token)
	}
}

func TestCommandWaitResumesFromCursor(t *testing.T) {
	var base string
	var calls []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	mux.HandleFunc("/exec/cmd_1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.RawQuery)
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Dropped mid-command: no exit event.
			io.WriteString(w, "id: 0-0\nevent: start\ndata: {\"id\":\"cmd_1\"}\n\nid: 3-0\nevent: stdout\ndata: {\"data\":\"one\"}\n\n")
			return
		}
		io.WriteString(w, "id: 6-0\nevent: stdout\ndata: {\"data\":\"two\"}\n\nid: 6-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"timedOut\":false}\n\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
	res, err := c.Sandbox("sbx_1").Command("cmd_1").Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "onetwo" || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if len(calls) != 2 || calls[0] != "" || calls[1] != "since=3-0" {
		t.Fatalf("resume queries %v", calls)
	}
}

// TestPtyReconnects drops the first socket after one frame and checks the
// client re-attaches the same session with the same clientId and resume=1,
// keeps the output stream intact, and ends cleanly on the exit frame.
func TestPtyReconnects(t *testing.T) {
	var base string
	var dials atomic.Int32
	type dial struct{ session, client, resume, proto string }
	seen := make(chan dial, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt_pty"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ptySubprotocol}})
		if err != nil {
			return
		}
		q := r.URL.Query()
		seen <- dial{q.Get("session"), q.Get("clientId"), q.Get("resume"), r.Header.Get("Sec-WebSocket-Protocol")}
		ctx := context.Background()
		// readInput skips size frames and returns the next keystrokes.
		readInput := func() (string, bool) {
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return "", false
				}
				var frame map[string]any
				json.Unmarshal(data, &frame)
				if frame["t"] == "i" {
					d, _ := frame["d"].(string)
					return d, true
				}
			}
		}
		if dials.Add(1) == 1 {
			conn.Write(ctx, websocket.MessageText, []byte(`{"t":"o","d":"hello "}`))
			readInput()
			conn.CloseNow() // abrupt drop
			return
		}
		conn.Write(ctx, websocket.MessageText, []byte(`{"t":"o","d":"back "}`))
		input, ok := readInput()
		if !ok {
			return
		}
		conn.Write(ctx, websocket.MessageText, []byte(`{"t":"o","d":"got `+input+`"}`))
		conn.Write(ctx, websocket.MessageText, []byte(`{"t":"x","code":7}`))
		conn.Close(websocket.StatusNormalClosure, "")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	c := New("zb_x", WithBaseURL(server.URL), WithPtyBackoff(time.Millisecond, 2*time.Millisecond))
	ctx := context.Background()
	pty, err := c.Sandbox("sbx_1").Pty.Create(ctx, PtyOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	first := <-seen
	buf := make([]byte, 6)
	if _, err := io.ReadFull(pty, buf); err != nil || string(buf) != "hello " {
		t.Fatalf("first read %q %v", buf, err)
	}
	if _, err := pty.Write([]byte("ls")); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, 5)
	if _, err := io.ReadFull(pty, back); err != nil || string(back) != "back " {
		t.Fatalf("after reconnect %q %v", back, err)
	}
	if _, err := pty.Write([]byte("pwd")); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(pty)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	second := <-seen
	if string(rest) != "got pwd" {
		t.Fatalf("rest %q", rest)
	}
	if !strings.HasPrefix(first.session, "bash-") || first.session != second.session || first.client != second.client {
		t.Fatalf("session/client changed across reconnect: %+v %+v", first, second)
	}
	if first.resume != "" || second.resume != "1" {
		t.Fatalf("resume flags %q %q", first.resume, second.resume)
	}
	// The box splits the header on commas and trims each entry.
	if strings.ReplaceAll(first.proto, " ", "") != "tribes.session,bt_pty" {
		t.Fatalf("subprotocol %q", first.proto)
	}
	if code := pty.ExitCode(); code == nil || *code != 7 {
		t.Fatalf("exit code %v", code)
	}
	if _, err := pty.Write([]byte("x")); !errors.Is(err, io.EOF) {
		t.Fatalf("write after exit: %v", err)
	}
	_ = pty.Detach()
}

func TestSandboxDetailJSONOmitsBoxToken(t *testing.T) {
	token, base := "bt_secret", "https://demo.zbox.sh"
	d := SandboxDetail{Sandbox: Sandbox{ID: "sbx_1", Status: StatusRunning}, Ready: true, BaseURL: &base, BoxToken: &token}
	for _, v := range []any{d, &d} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), token) || strings.Contains(string(data), "boxToken") {
			t.Fatalf("token in JSON: %s", data)
		}
		if !strings.Contains(string(data), `"id":"sbx_1"`) || !strings.Contains(string(data), `"baseUrl"`) {
			t.Fatalf("detail lost fields: %s", data)
		}
	}
	var back SandboxDetail
	if err := json.Unmarshal([]byte(`{"id":"sbx_1","status":"running","ready":true,"boxToken":"bt_x"}`), &back); err != nil || back.BoxToken == nil || *back.BoxToken != "bt_x" {
		t.Fatalf("decoding must still read the token: %v %+v", err, back)
	}
}

func TestGetOrCreateStartsAnArchivingBox(t *testing.T) {
	var base string
	var calls []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"sbx_1","name":"demo","status":"stopping","isArchived":true,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
	})
	mux.HandleFunc("/sandbox/sbx_1/restore", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		io.WriteString(w, `{"success":true}`)
	})
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	box, err := New("zb_x", WithBaseURL(server.URL)).Sandboxes.GetOrCreate(context.Background(), "demo", CreateOptions{})
	if err != nil || box.ID != "sbx_1" {
		t.Fatalf("%v %v", box, err)
	}
	if len(calls) != 1 || calls[0] != "POST /sandbox/sbx_1/restore" {
		t.Fatalf("calls %v", calls)
	}
}

// GetOrCreate creates a missing box with every create option on the claim.
func TestGetOrCreateSendsEveryCreateOptionOnTheClaim(t *testing.T) {
	var base string
	var claim map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/sandbox/identity/claim", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{"sandboxId":"sbx_1","created":true}`)
	})
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	opts := CreateOptions{Tier: "l", SSHPublicKey: "ssh-ed25519 AAAAtest", Mailbox: true, Private: true}
	box, err := New("zb_x", WithBaseURL(server.URL)).Sandboxes.GetOrCreate(context.Background(), "ci-runner", opts)
	if err != nil || box.ID != "sbx_1" {
		t.Fatalf("%v %v", box, err)
	}
	want := map[string]any{"name": "ci-runner", "tier": "l", "sshPublicKey": "ssh-ed25519 AAAAtest", "mailbox": true, "private": true}
	if fmt.Sprint(claim) != fmt.Sprint(want) {
		t.Fatalf("claim body %v, want %v", claim, want)
	}
}

// The options apply only on create: a box that already has the name is returned
// as it is, with no claim and no tier change.
func TestGetOrCreateReturnsAnExistingBoxAsItIs(t *testing.T) {
	var base string
	var unexpected []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		unexpected = append(unexpected, r.Method+" "+r.URL.Path)
		http.NotFound(w, r)
	})
	mux.HandleFunc("/sandbox", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"sbx_1","name":"ci-runner","status":"running","tier":"s","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
	})
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	box, err := New("zb_x", WithBaseURL(server.URL)).Sandboxes.GetOrCreate(context.Background(), "ci-runner", CreateOptions{Tier: "l"})
	if err != nil || box.ID != "sbx_1" {
		t.Fatalf("%v %v", box, err)
	}
	if len(unexpected) != 0 {
		t.Fatalf("unexpected calls %v", unexpected)
	}
}

func TestGetOrCreateRejectsADifferentOptionsName(t *testing.T) {
	_, err := New("zb_x", WithBaseURL("http://127.0.0.1:1")).Sandboxes.GetOrCreate(context.Background(), "ci-runner", CreateOptions{Name: "other"})
	if !IsCode(err, CodeInvalidRequest) {
		t.Fatalf("err %v", err)
	}
}

// A closed stream stays closed: a drop after Close is not re-attached.
func TestExecStreamCloseStopsTheResume(t *testing.T) {
	var base string
	var resumed int32
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 0-0\nevent: start\ndata: {\"id\":\"cmd_1\"}\n\nid: 3-0\nevent: stdout\ndata: {\"data\":\"one\"}\n\n")
	})
	mux.HandleFunc("/exec/cmd_1/events", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&resumed, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 6-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"timedOut\":false}\n\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
	stream, err := c.Sandbox("sbx_1").ExecStream(context.Background(), "echo one; sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Next() || stream.Event().Type != EventStart || !stream.Next() || stream.Event().Type != EventStdout {
		t.Fatalf("expected start and stdout, got %+v err %v", stream.Event(), stream.Err())
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if stream.Next() || !IsCode(stream.Err(), CodeUnavailable) {
		t.Fatalf("Next after Close: %+v err %v", stream.Event(), stream.Err())
	}
	if n := atomic.LoadInt32(&resumed); n != 0 {
		t.Fatalf("re-attached %d times after Close", n)
	}
}

// TestPtyResendsSizeWhenOwned: a socket that attaches to a session another
// client holds is blocked, so the box drops its first size frame. After the
// takeover the box says `owned`, and the size must be sent again.
func TestPtyResendsSizeWhenOwned(t *testing.T) {
	var base string
	frames := make(chan map[string]any, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ptySubprotocol}})
		if err != nil {
			return
		}
		ctx := context.Background()
		conn.Write(ctx, websocket.MessageText, []byte(`{"t":"blocked","holder":{"clientId":"browser","label":"tab"}}`))
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var frame map[string]any
			json.Unmarshal(data, &frame)
			frames <- frame
			if frame["t"] == "takeover" {
				conn.Write(ctx, websocket.MessageText, []byte(`{"t":"owned","holder":{"clientId":"me","label":"zipbox-sdk"}}`))
			}
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	pty, err := New("zb_x", WithBaseURL(server.URL)).Sandbox("sbx_1").Pty.Attach(context.Background(), "bash-x", PtyOptions{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Detach()
	var seen []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-frames:
			seen = append(seen, fmt.Sprint(f["t"]))
			if f["t"] == "r" && len(seen) > 2 {
				if f["c"] != float64(120) || f["r"] != float64(40) {
					t.Fatalf("size frame %v", f)
				}
				// size (dropped while blocked), takeover, size again
				if strings.Join(seen, ",") != "r,takeover,r" {
					t.Fatalf("frames %v", seen)
				}
				return
			}
		case <-timeout:
			t.Fatalf("no size frame after owned; frames %v", seen)
		}
	}
}

func TestPtySocketURLKeepsBasePath(t *testing.T) {
	p := &Pty{SessionID: "bash-x", ClientID: "c1", label: "zipbox-sdk"}
	got, err := p.socketURL("https://term.example.test/42001", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "wss://term.example.test/42001/?") || !strings.Contains(got, "resume=1") {
		t.Fatalf("socketURL = %s", got)
	}
	got, _ = p.socketURL("https://demo.zbox.sh", false)
	if !strings.HasPrefix(got, "wss://demo.zbox.sh/?") {
		t.Fatalf("socketURL = %s", got)
	}
}

func TestScreenshotUsesTheImagesFfmpeg(t *testing.T) {
	// The platform image has no ImageMagick; pipefail turns a failed capture into a
	// non-zero exit instead of an empty PNG.
	if !strings.HasPrefix(screenshotCommand, "set -o pipefail;") ||
		!strings.Contains(screenshotCommand, "ffmpeg -loglevel error -f x11grab -i :10 ") ||
		strings.Contains(screenshotCommand, "import ") {
		t.Fatalf("screenshotCommand = %s", screenshotCommand)
	}
}

func TestPreviewApexPreservesTransportOrigin(t *testing.T) {
	cases := map[string]string{
		"https://box--demo.zbox.sh":      "demo.zbox.sh",
		"https://box--box--demo.zbox.sh": "box--demo.zbox.sh",
		// A private box serves itself on box.<apex>; only one prefix is ever dropped.
		"https://box.demo.zbox.sh":      "demo.zbox.sh",
		"https://box--box.zbox.sh":      "box.zbox.sh",
		"https://demo.zbox.sh":          "demo.zbox.sh",
		"https://mailbox--demo.zbox.sh": "mailbox--demo.zbox.sh",
		"http://127.0.0.1:9100":         "127.0.0.1",
	}
	for origin, apex := range cases {
		t.Run(origin, func(t *testing.T) {
			client := New("key")
			client.creds["box"] = &boxCreds{baseURL: origin, token: "bt_test"}
			host, err := client.Sandbox("box").host(context.Background())
			if err != nil || host != apex {
				t.Fatalf("host = %q, %v; want %q", host, err, apex)
			}
			if client.creds["box"].baseURL != origin {
				t.Fatal("preview changed the transport origin")
			}
		})
	}
}

func TestPreviewPendingBeyondExecDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("checks real provider Retry-After beyond the30s exec window")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Argv []string `json:"argv"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.Join(body.Argv, " ") != "tribes-caddy add web.127.0.0.1 127.0.0.1:4000 --no-wait" {
			t.Errorf("unexpected command: %v", body.Argv)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "event: start\ndata: {\"id\":\"p\"}\n\nevent: stdout\ndata: {\"data\":\"{\\\"status\\\":\\\"pending\\\",\\\"retryAfterMs\\\":31000}\"}\n\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":75}\n\n")
		} else {
			fmt.Fprint(w, "event: start\ndata: {\"id\":\"p\"}\n\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n")
		}
	}))
	defer server.Close()
	client := New("key", WithBackoff(time.Millisecond, time.Millisecond))
	client.creds["box"] = &boxCreds{baseURL: server.URL, token: "bt"}
	start := time.Now()
	preview, err := client.Sandbox("box").Preview.Create(context.Background(), 4000, "web")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 31*time.Second {
		t.Fatalf("returned readiness before provider delay: %v", elapsed)
	}
	if calls.Load() != 2 || preview.URL != "https://web.127.0.0.1" {
		t.Fatalf("calls=%d preview=%+v", calls.Load(), preview)
	}
}

func TestForkOptionsNeverPrintTheAuthKey(t *testing.T) {
	const key = "tskey-auth-kUnitTest1CNTRL-NotARealKey0123456789"
	opts := ForkOptions{Name: "demo-2", SourcePolicy: ForkPauseAndResume, TailscaleAuthKey: key}
	nested := struct{ Opts ForkOptions }{opts}
	var printed []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		printed = append(printed, fmt.Sprintf(verb, opts), fmt.Sprintf(verb, &opts), fmt.Sprintf(verb, nested))
	}
	printed = append(printed, opts.String(), opts.GoString(), fmt.Sprint(opts), fmt.Sprintln(opts))
	data, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	printed = append(printed, string(data))
	for _, out := range printed {
		if strings.Contains(out, key) || strings.Contains(out, "tskey-") {
			t.Fatalf("printed the auth key: %s", out)
		}
	}
	if got := fmt.Sprintf("%+v", opts); !strings.Contains(got, "demo-2") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("%%+v = %s", got)
	}
	if got := fmt.Sprintf("%+v", ForkOptions{Name: "demo-2"}); strings.Contains(got, "[redacted]") {
		t.Fatalf("an absent key reads as set: %s", got)
	}
}

func TestForkOptionsPrintPrivate(t *testing.T) {
	opts := ForkOptions{Name: "demo-2", SourcePolicy: ForkRequirePaused, Private: true}
	if got := opts.String(); got != "{Name:demo-2 SourcePolicy:require_paused Private:true TailscaleAuthKey: NoWait:false}" {
		t.Fatalf("String = %s", got)
	}
	if got := opts.GoString(); got != `zipbox.ForkOptions{Name:"demo-2", SourcePolicy:"require_paused", Private:true, TailscaleAuthKey:"", NoWait:false}` {
		t.Fatalf("GoString = %s", got)
	}
	if got := fmt.Sprintf("%+v", opts); !strings.Contains(got, "Private:true") {
		t.Fatalf("%%+v = %s", got)
	}
	if got := fmt.Sprintf("%v", ForkOptions{Name: "demo-2"}); !strings.Contains(got, "Private:false") {
		t.Fatalf("%%v = %s", got)
	}
}

// A blank key is refused before anything is sent, so no fork is made.
func TestForkRefusesABlankAuthKeyBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer server.Close()
	c := New("zb_test", WithBaseURL(server.URL))
	_, err := c.Sandboxes.Fork(context.Background(), "sbx_1", ForkOptions{Name: "demo-2", SourcePolicy: ForkRequirePaused, TailscaleAuthKey: "  \n"})
	if !IsCode(err, CodeInvalidRequest) || calls.Load() != 0 {
		t.Fatalf("err %v, %d requests", err, calls.Load())
	}
}

// A ctx that ends while Fork waits stops only the wait: nothing cancels the fork.
func TestForkWaitEndsWithCtxAndLeavesTheForkRunning(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /sandbox/sbx_1/fork":
			w.WriteHeader(202)
			io.WriteString(w, `{"operationId":"fork_1","sandbox":{"id":"sbx_2","name":"demo-2","status":"provisioning","createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z"},"fork":{"operationId":"fork_1","sourceSandboxId":"sbx_1","childSandboxId":"sbx_2","sourcePolicy":"require_paused","phase":"queued","sourceRecovery":"not_needed","attempt":0,"createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z"}}`)
		case "GET /sandbox/sbx_2":
			io.WriteString(w, `{"id":"sbx_2","name":"demo-2","status":"provisioning","createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z","ready":false}`)
		default:
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	c := New("zb_test", WithBaseURL(server.URL), WithPollInterval(time.Millisecond, 2*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Sandboxes.Fork(ctx, "sbx_1", ForkOptions{Name: "demo-2", SourcePolicy: ForkRequirePaused})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "POST /sandbox/sbx_1/fork" {
		t.Fatalf("requests %v", seen)
	}
	for _, s := range seen[1:] {
		if s != "GET /sandbox/sbx_2" {
			t.Fatalf("the wait sent %s", s)
		}
	}
}

// The conformance matcher's {{SAME}} rejects a header that changed since the previous request.
func TestConformanceSameHeaderPlaceholder(t *testing.T) {
	var ex exchange
	ex.Request.Method, ex.Request.Path = "POST", "/sandbox/sbx_1/fork"
	ex.Request.Headers = map[string]string{"idempotency-key": "{{SAME}}"}
	req := httptest.NewRequest("POST", "/sandbox/sbx_1/fork", nil)
	req.Header.Set("Idempotency-Key", "k1")
	if p := matchRequest(ex, req, nil, http.Header{"Idempotency-Key": {"k1"}}); p != "" {
		t.Fatalf("same key rejected: %s", p)
	}
	if p := matchRequest(ex, req, nil, http.Header{"Idempotency-Key": {"k2"}}); p == "" {
		t.Fatal("a changed key passed")
	}
	if p := matchRequest(ex, req, nil, nil); p == "" {
		t.Fatal("{{SAME}} on a first request passed")
	}
}

// audit-ax AX-03: a box call's error follows the server's readiness. A paused box is
// not retryable, so a follower ends instead of spinning, and a private box awaiting
// its tailnet names the remedy.
func TestBoxNotReadyFollowsReadiness(t *testing.T) {
	cases := []struct {
		readiness, status string
		code              ErrorCode
		retryable         bool
	}{
		{"settling", "archived", CodeBoxNotRunning, true},
		{"paused", "archived", CodeBoxNotRunning, false},
		{"start_failed", "archived", CodeBoxNotRunning, false},
		{"awaits_tailnet", "running", CodeTailnetNotConnected, false},
	}
	for _, tc := range cases {
		var boxCalls atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/sandbox/sbx_1", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":"sbx_1","status":%q,"readiness":%q,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":false}`, tc.status, tc.readiness)
		})
		mux.HandleFunc("/exec/cmd_1/events", func(w http.ResponseWriter, r *http.Request) { boxCalls.Add(1) })
		server := httptest.NewServer(mux)
		c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := c.Sandbox("sbx_1").Command("cmd_1").Wait(ctx)
		cancel()
		server.Close()
		var zerr *Error
		if tc.retryable {
			// A settling box comes back by itself: the follower keeps its ladder.
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: want the follower still waiting, got %v", tc.readiness, err)
			}
			continue
		}
		if !errors.As(err, &zerr) || zerr.Code != tc.code || zerr.Retryable {
			t.Errorf("%s: got %v", tc.readiness, err)
		}
		if boxCalls.Load() != 0 {
			t.Errorf("%s: the box was called", tc.readiness)
		}
	}
}

// audit-ax AX-10: a read that shows the box not ready, and every lifecycle call,
// drop its cached credentials, so the next box call never dials the old address.
func TestNotReadyReadAndLifecycleCallsDropCachedCredentials(t *testing.T) {
	var base string
	var mu sync.Mutex
	ready := true
	var execCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if ready {
			detailHandler(&base, "bt")(w, r)
			return
		}
		io.WriteString(w, `{"id":"sbx_1","status":"archived","readiness":"paused","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":false}`)
	})
	for _, action := range []string{"/restore", "/reset", "/tier"} {
		mux.HandleFunc("/sandbox/sbx_1"+action, func(w http.ResponseWriter, r *http.Request) {})
	}
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		execCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 1-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	ctx := context.Background()
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
	box := c.Sandbox("sbx_1")
	setReady := func(value bool) {
		mu.Lock()
		ready = value
		mu.Unlock()
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"Refresh", func() error { _, err := box.Refresh(ctx); return err }},
		{"Start", func() error { return box.Start(ctx) }},
		{"Restart", func() error { return box.Restart(ctx) }},
		{"SetTier", func() error { return box.SetTier(ctx, "m") }},
	} {
		setReady(true)
		if _, err := box.Exec(ctx, "true"); err != nil {
			t.Fatalf("%s: warm-up exec: %v", step.name, err)
		}
		setReady(false)
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		before := execCalls.Load()
		if _, err := box.Exec(ctx, "true"); !IsCode(err, CodeBoxNotRunning) {
			t.Fatalf("%s: want box_not_running from a fresh read, got %v", step.name, err)
		}
		if execCalls.Load() != before {
			t.Fatalf("%s: dialled the box's old address", step.name)
		}
	}
}

// audit-ax AX-23: a follower's ladder never retries sooner than the server asked.
func TestFollowerWaitsOutRetryAfter(t *testing.T) {
	var base string
	var mu sync.Mutex
	var opened []time.Time
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox/sbx_1", detailHandler(&base, "bt"))
	mux.HandleFunc("/exec/cmd_1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		opened = append(opened, time.Now())
		n := len(opened)
		mu.Unlock()
		if n == 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"busy"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Dropped mid-command: no exit event.
			io.WriteString(w, "id: 0-0\nevent: start\ndata: {\"id\":\"cmd_1\"}\n\n")
			return
		}
		io.WriteString(w, "id: 6-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	c := New("zb_x", WithBaseURL(server.URL), WithMaxRetries(0), WithBackoff(time.Millisecond, 2*time.Millisecond))
	if _, err := c.Sandbox("sbx_1").Command("cmd_1").Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 3 {
		t.Fatalf("opened %d times", len(opened))
	}
	if gap := opened[2].Sub(opened[1]); gap < 900*time.Millisecond {
		t.Fatalf("re-attached %v after a Retry-After of 1s", gap)
	}
}

// audit-ax AX-17: one retry rule in every SDK. An envelope that says it is not
// retryable is never retried, a 502 on a GET is, and a code this SDK does not know
// maps by status.
func TestOneRetryRule(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"code":"unavailable","message":"no","retryable":false}}`)
	}))
	defer server.Close()
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
	if _, err := c.Sandboxes.List(context.Background()); !IsCode(err, CodeUnavailable) || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
	resp := &http.Response{StatusCode: 409, Header: http.Header{}}
	zerr := errorFromResponse(resp, []byte(`{"error":{"code":"a_code_from_later","message":"m"}}`))
	if zerr.Code != CodeConflict || zerr.DocsURL != "https://zipbox.ai/docs/errors#conflict" {
		t.Fatalf("%+v", zerr)
	}
}
