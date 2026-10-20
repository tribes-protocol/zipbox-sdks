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

func TestExecStreamEndsBeforeExitIsUnavailable(t *testing.T) {
	body := io.NopCloser(strings.NewReader("id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 2-0\nevent: stdout\ndata: {\"data\":\"hi\"}\n\n"))
	s := newExecStream(body, "0-0")
	var types []ExecEventType
	for s.Next() {
		types = append(types, s.Event().Type)
	}
	if len(types) != 2 || s.Cursor() != "2-0" || !IsCode(s.Err(), CodeUnavailable) {
		t.Fatalf("types %v cursor %s err %v", types, s.Cursor(), s.Err())
	}
}

func TestBackoffLadder(t *testing.T) {
	c := New("k", WithBackoff(500*time.Millisecond, 30*time.Second))
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := c.ladder(i, c.backoffBase, c.backoffMax); got != w {
			t.Errorf("ladder(%d) = %v, want %v", i, got, w)
		}
		for j := 0; j < 50; j++ {
			if d := c.backoff(i); d < w/2 || d >= w {
				t.Fatalf("backoff(%d) = %v outside [%v, %v)", i, d, w/2, w)
			}
		}
	}
	if got := c.ladder(1000, c.backoffBase, c.backoffMax); got != 30*time.Second {
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
	c := New("zb_x", WithBaseURL(server.URL), WithBackoff(time.Millisecond, 2*time.Millisecond))
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
	box, err := New("zb_x", WithBaseURL(server.URL)).Sandboxes.GetOrCreate(context.Background(), "demo")
	if err != nil || box.ID != "sbx_1" {
		t.Fatalf("%v %v", box, err)
	}
	if len(calls) != 1 || calls[0] != "POST /sandbox/sbx_1/restore" {
		t.Fatalf("calls %v", calls)
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
