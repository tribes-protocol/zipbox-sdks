package cli

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT"

// fakeZipbox serves the control plane and one box from one server.
func fakeZipbox(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			want := "Bearer " + testKey
			if strings.HasPrefix(r.URL.Path, "/exec") {
				want = "Bearer bt_cli"
			}
			if r.Header.Get("Authorization") != want {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad key","requestId":"r","retryable":false,"docsUrl":"https://zipbox.ai/docs/errors#unauthorized"}}`)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/sandbox", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"sbx_1","name":"demo","status":"running","tier":"s","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
	}))
	mux.HandleFunc("/sandbox/sbx_1", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"sbx_1","name":"demo","status":"running","ipv6":"2a01::1","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_cli"}`, server.URL)
	}))
	mux.HandleFunc("/sandbox/identity/claim", auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":"machine_limit_reached","message":"You've reached this account's limit of 250 machines.","requestId":"r","retryable":false,"docsUrl":"https://zipbox.ai/docs/errors#machine_limit_reached"}}`)
	}))
	mux.HandleFunc("/account/whoami", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"userId":"did:privy:u1","apiKey":{"id":"k1","name":"laptop","kind":"cli","last4":"TTTT","createdAt":"2026-10-19T00:00:00Z"}}`)
	}))
	mux.HandleFunc("/exec", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: 0-0\nevent: start\ndata: {\"id\":\"cmd_1\"}\n\n")
		fmt.Fprintf(w, "id: 4-0\nevent: stdout\ndata: {\"data\":%q}\n\n", "ran "+fmt.Sprint(body["cmd"])+"\n")
		fmt.Fprintf(w, "id: 4-5\nevent: stderr\ndata: {\"data\":\"oops\\n\"}\n\n")
		fmt.Fprintf(w, "id: 4-5\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":3,\"timedOut\":false}\n\n")
	}))
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

type harness struct {
	app      *app
	out, err *bytes.Buffer
	env      map[string]string
}

func newHarness(t *testing.T, apiURL string, stdin string) *harness {
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, env: map[string]string{
		"ZIPBOX_API_URL":    apiURL,
		"ZIPBOX_CONFIG_DIR": t.TempDir(),
		"HOME":              t.TempDir(),
	}}
	h.app = &app{
		in:              strings.NewReader(stdin),
		out:             h.out,
		err:             h.err,
		getenv:          func(k string) string { return h.env[k] },
		http:            &http.Client{},
		browser:         func(string) error { return nil },
		exePath:         "/usr/local/bin/zipbox",
		stdinIsTerminal: func() bool { return false },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.app.jsonOut = false
	return h.app.run(context.Background(), args)
}

func TestExecPropagatesRemoteExitStatus(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("exec", "demo", "--", "make", "test"); code != 3 {
		t.Fatalf("exit %d, stderr %s", code, h.err)
	}
	if h.out.String() != "ran make test\n" || h.err.String() != "oops\n" {
		t.Fatalf("stdout %q stderr %q", h.out, h.err)
	}
}

func TestJSONShapes(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey

	if code := h.run("exec", "sbx_1", "--json", "--", "true"); code != 3 {
		t.Fatalf("exit %d", code)
	}
	var result map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &result); err != nil {
		t.Fatalf("exec --json: %v: %s", err, h.out)
	}
	for _, key := range []string{"id", "exitCode", "stdout", "stderr", "timedOut"} {
		if _, ok := result[key]; !ok {
			t.Errorf("exec --json missing %q: %v", key, result)
		}
	}
	if result["stdout"] != "ran true\n" || result["exitCode"] != float64(3) {
		t.Errorf("exec --json %v", result)
	}

	h.out.Reset()
	if code := h.run("sandbox", "list", "--json"); code != 0 {
		t.Fatalf("list exit %d: %s", code, h.err)
	}
	var boxes []map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &boxes); err != nil || len(boxes) != 1 || boxes[0]["id"] != "sbx_1" || boxes[0]["status"] != "running" {
		t.Fatalf("sandbox list --json: %v %s", err, h.out)
	}

	h.out.Reset()
	if code := h.run("sandbox", "get", "demo", "--json"); code != 0 {
		t.Fatalf("get exit %d: %s", code, h.err)
	}
	if strings.Contains(h.out.String(), "bt_cli") || strings.Contains(h.out.String(), "boxToken") {
		t.Fatalf("sandbox get --json printed the box token: %s", h.out)
	}

	h.out.Reset()
	if code := h.run("ssh", "demo", "--json"); code != 0 {
		t.Fatalf("ssh exit %d: %s", code, h.err)
	}
	var ssh map[string]string
	json.Unmarshal(h.out.Bytes(), &ssh)
	if ssh["command"] != "ssh root@2a01::1" || ssh["host"] != "2a01::1" || ssh["user"] != "root" {
		t.Fatalf("ssh --json %s", h.out)
	}
}

func TestErrorsPrintDocsURL(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = "zb_" + strings.Repeat("X", 43)
	if code := h.run("sandbox", "list"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.err.String(), "bad key (unauthorized)") || !strings.Contains(h.err.String(), "https://zipbox.ai/docs/errors#unauthorized") {
		t.Fatalf("stderr %q", h.err)
	}
}

// AX-09: a failure says whether a retry may succeed and which request failed, in
// both outputs. It still exits 1, like every failure of the CLI itself.
func TestErrorsSayWhetherToRetryAndTheRequestID(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = "zb_" + strings.Repeat("X", 43)
	if code := h.run("sandbox", "list"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if stderr := h.err.String(); !strings.Contains(stderr, "  request id: r\n") || !strings.Contains(stderr, "  retryable: no\n") {
		t.Fatalf("stderr %q", stderr)
	}
	h.err.Reset()
	if code := h.run("sandbox", "list", "--json"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(h.err.Bytes(), &out); err != nil || out.Error["requestId"] != "r" || out.Error["retryable"] != false {
		t.Fatalf("stderr %s (%v)", h.err, err)
	}
}

// A create with a NEW name at the account's machine limit prints the reuse remedy
// and its docs line, and exits 1 without retrying.
func TestCreateAtMachineLimitPrintsRemedy(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("sandbox", "create", "--name", "worker-251"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	stderr := h.err.String()
	if !strings.Contains(stderr, "limit of 250 machines. (machine_limit_reached)") || !strings.Contains(stderr, "docs: https://zipbox.ai/docs/errors#machine_limit_reached") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestNotLoggedIn(t *testing.T) {
	h := newHarness(t, "http://127.0.0.1:1", "")
	if code := h.run("sandbox", "list"); code != 1 || !strings.Contains(h.err.String(), "zipbox login") {
		t.Fatalf("exit %d stderr %q", code, h.err)
	}
}

func TestLoginWithKeySavesAndVerifies(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, testKey+"\n")
	if code := h.run("login", "--with-key"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.out.String(), "did:privy:u1") {
		t.Fatalf("stdout %q", h.out)
	}
	path := filepath.Join(h.env["ZIPBOX_CONFIG_DIR"], "config.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config %v %v", info, err)
	}
	h.out.Reset()
	if code := h.run("whoami"); code != 0 || !strings.Contains(h.out.String(), "laptop") {
		t.Fatalf("whoami %d %q %q", code, h.out, h.err)
	}
	bad := newHarness(t, server.URL, "not-a-key\n")
	if code := bad.run("login", "--with-key"); code != 1 {
		t.Fatalf("bad key accepted")
	}
}

func TestLoginCallback(t *testing.T) {
	srv := &loginServer{state: "s3cret", origin: "https://zipbox.ai", keys: make(chan string, 1)}
	server := httptest.NewServer(srv)
	defer server.Close()

	pre, _ := http.NewRequest("OPTIONS", server.URL+"/callback", nil)
	pre.Header.Set("Origin", "https://zipbox.ai")
	pre.Header.Set("Access-Control-Request-Private-Network", "true")
	resp, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://zipbox.ai" ||
		resp.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight %d %v", resp.StatusCode, resp.Header)
	}

	post := func(origin, state, key string) int {
		body := fmt.Sprintf(`{"state":%q,"key":%q}`, state, key)
		req, _ := http.NewRequest("POST", server.URL+"/callback", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("https://zipbox.ai", "wrong", testKey); code != 403 {
		t.Fatalf("state mismatch answered %d", code)
	}
	if code := post("https://evil.example", "s3cret", testKey); code != 403 {
		t.Fatalf("foreign origin answered %d", code)
	}
	if code := post("https://zipbox.ai", "s3cret", "zb_short"); code != 400 {
		t.Fatalf("malformed key answered %d", code)
	}
	select {
	case <-srv.keys:
		t.Fatal("a rejected request delivered a key")
	default:
	}
	if code := post("https://zipbox.ai", "s3cret", testKey); code != 200 {
		t.Fatalf("valid callback answered %d", code)
	}
	if got := <-srv.keys; got != testKey {
		t.Fatalf("key %q", got)
	}
	if code := post("https://zipbox.ai", "s3cret", testKey); code != 409 {
		t.Fatalf("a second callback answered %d, want 409", code)
	}
	select {
	case <-srv.keys:
		t.Fatal("a second callback delivered a key")
	default:
	}
}

func TestBrowserLoginEndToEnd(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_WEB_URL"] = "https://zipbox.ai"
	// The "browser" is the cli/auth page: it posts the key to the callback.
	h.app.browser = func(target string) error {
		go func() {
			var port, state string
			fmt.Sscanf(strings.ReplaceAll(strings.SplitN(target, "?", 2)[1], "&", " "), "port=%s state=%s", &port, &state)
			body := fmt.Sprintf(`{"state":%q,"key":%q}`, state, testKey)
			req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/callback", strings.NewReader(body))
			req.Header.Set("Origin", "https://zipbox.ai")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	if code := h.run("login"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "https://zipbox.ai/cli/auth?port=") || !strings.Contains(h.out.String(), "did:privy:u1") {
		t.Fatalf("stderr %q stdout %q", h.err, h.out)
	}
}

func TestMCPBridge(t *testing.T) {
	var mu sync.Mutex
	var sessions []string
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(401)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(200)
			return
		}
		var msg map[string]any
		json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		sessions = append(sessions, fmt.Sprint(msg["method"])+"|"+r.Header.Get("Mcp-Session-Id")+"|"+r.Header.Get("MCP-Protocol-Version"))
		mu.Unlock()
		switch msg["method"] {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, msg["id"])
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\n")
			fmt.Fprintf(w, "data: \"id\":%v,\"result\":{\"tools\":[{\"name\":\"execute_command\"}]}}\n\n", msg["id"])
		default:
			w.WriteHeader(500)
			io.WriteString(w, "boom")
		}
	}))
	defer mcp.Close()

	// Sequential stdin: each line is sent after the previous answer arrives, as
	// a real client does after initialize.
	inR, inW := io.Pipe()
	out := &syncBuffer{}
	bridge := &mcpBridge{url: mcp.URL, key: testKey, http: &http.Client{}}
	done := make(chan error, 1)
	go func() { done <- bridge.run(context.Background(), inR, out) }()
	send := func(line string, wantLines int) {
		io.WriteString(inW, line+"\n")
		for out.lines() < wantLines {
			time.Sleep(time.Millisecond)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, 1)
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 2)
	send(`{"jsonrpc":"2.0","id":3,"method":"explode"}`, 3)
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) || !strings.Contains(lines[1], `"execute_command"`) {
		t.Fatalf("lines %q", lines)
	}
	if !strings.Contains(lines[2], `"id":3`) || !strings.Contains(lines[2], "HTTP 500") {
		t.Fatalf("error line %q", lines[2])
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"initialize||", "notifications/initialized|sess-1|2025-06-18", "tools/list|sess-1|2025-06-18", "explode|sess-1|2025-06-18"}
	if strings.Join(sessions, ",") != strings.Join(want, ",") {
		t.Fatalf("requests %v, want %v", sessions, want)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) lines() int { return strings.Count(s.String(), "\n") }

func TestMCPInit(t *testing.T) {
	h := newHarness(t, "", "")
	home := h.env["HOME"]
	cursor := filepath.Join(home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(cursor), 0o755)
	os.WriteFile(cursor, []byte(`{"mcpServers":{"other":{"command":"x"}},"theme":"dark"}`), 0o644)
	if code := h.run("mcp", "init", "cursor"); code != 0 {
		t.Fatalf("exit %d %s", code, h.err)
	}
	var doc map[string]any
	data, _ := os.ReadFile(cursor)
	json.Unmarshal(data, &doc)
	servers := doc["mcpServers"].(map[string]any)
	zb := servers["zipbox"].(map[string]any)
	if servers["other"] == nil || doc["theme"] != "dark" || zb["command"] != "/usr/local/bin/zipbox" {
		t.Fatalf("cursor config %s", data)
	}

	codex := filepath.Join(home, ".codex", "config.toml")
	os.MkdirAll(filepath.Dir(codex), 0o755)
	os.WriteFile(codex, []byte("model = \"o4\"\n\n[mcp_servers.zipbox]\nurl = \"old\"\n\n[mcp_servers.zipbox.env]\nA = \"1\"\n\n[other]\nx = 1\n"), 0o644)
	if code := h.run("mcp", "init", "codex"); code != 0 {
		t.Fatalf("exit %d %s", code, h.err)
	}
	data, _ = os.ReadFile(codex)
	text := string(data)
	if strings.Contains(text, "old") || strings.Contains(text, "A = ") || !strings.Contains(text, "[other]") || !strings.Contains(text, `model = "o4"`) ||
		!strings.Contains(text, `url = "https://mcp.zipbox.ai/mcp"`) || !strings.Contains(text, `bearer_token_env_var = "ZIPBOX_API_KEY"`) {
		t.Fatalf("codex config:\n%s", text)
	}

	h.out.Reset()
	if code := h.run("mcp", "init", "claude", "--json"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var claude map[string]string
	json.Unmarshal(h.out.Bytes(), &claude)
	if !strings.HasPrefix(claude["command"], "claude mcp add --transport http zipbox https://mcp.zipbox.ai/mcp") {
		t.Fatalf("claude %v", claude)
	}
}

// fakeFork serves a running source sbx_1 ("demo") and the box it forks into,
// sbx_2 ("demo-2"). The new box is private (with agent key priv) when private is
// set; failed makes its copy_failed fork remove it (it answers not_found).
type fakeFork struct {
	server  *httptest.Server
	priv    *ecdh.PrivateKey
	private bool
	failed  bool
	mu      sync.Mutex
	reqs    []recorded
	keys    []string
}

const forkRow = `"operationId":"fork_1","sourceSandboxId":"sbx_1","childSandboxId":"sbx_2","sourceRecovery":"pending","attempt":0,"createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z"`

func newFakeFork(t *testing.T, private, failed bool) *fakeFork {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	agentKey := base64.StdEncoding.EncodeToString(spki)
	f := &fakeFork{priv: priv, private: private, failed: failed}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, string(body)})
		if r.Method == "POST" {
			f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		}
		f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /sandbox":
			io.WriteString(w, `[{"id":"sbx_1","name":"demo","status":"running","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
		case "POST /sandbox/sbx_1/fork":
			var req struct {
				SourcePolicy string `json:"sourcePolicy"`
			}
			_ = json.Unmarshal(body, &req)
			if req.SourcePolicy != "pause_and_resume" {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":{"code":"pause_required","message":"demo is running.","requestId":"r","retryable":false,"docsUrl":"https://zipbox.ai/docs/errors#pause_required"}}`)
				return
			}
			w.WriteHeader(202)
			fmt.Fprintf(w, `{"operationId":"fork_1","sandbox":{"id":"sbx_2","name":"demo-2","status":"provisioning","privateMode":%t,"createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z"},"fork":{%s,"sourcePolicy":"pause_and_resume","phase":"queued"}}`, f.private, forkRow)
		// The readiness wait and a detail read answer the same row.
		case "GET /sandbox/sbx_2", "GET /sandbox/sbx_2/wait":
			switch {
			case f.failed:
				// The failed fork removed its box: the API answers not_found for it.
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"code":"not_found","message":"Sandbox not found","requestId":"r","retryable":false,"docsUrl":"https://zipbox.ai/docs/errors#not_found"}}`)
			case f.private:
				fmt.Fprintf(w, `{"id":"sbx_2","name":"demo-2","status":"running","privateMode":true,"agentPublicKey":%q,"createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z","ready":false}`, agentKey)
			default:
				fmt.Fprintf(w, `{"id":"sbx_2","name":"demo-2","status":"running","agentPublicKey":%q,"createdAt":"2026-11-05T00:00:00Z","updatedAt":"2026-11-05T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_cli"}`, agentKey, f.server.URL)
			}
		case "GET /sandbox/sbx_2/fork":
			fmt.Fprintf(w, `{%s,"sourcePolicy":"pause_and_resume","phase":"failed","error":{"code":"copy_failed","message":"The copy could not produce a verified filesystem."}}`, forkRow)
		case "PUT /sandbox/sbx_2/tailscale":
			w.WriteHeader(202)
			io.WriteString(w, `{"pending":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFork) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeFork) find(method, path string) *recorded {
	for _, r := range f.requests() {
		if r.method == method && r.path == path {
			return &r
		}
	}
	return nil
}

func (f *fakeFork) assertKeyNeverInClear(t *testing.T, h *harness) {
	t.Helper()
	for _, r := range f.requests() {
		if strings.Contains(r.body, testAuthKey) || strings.Contains(r.path, testAuthKey) {
			t.Fatalf("the auth key left in the clear: %s %s %s", r.method, r.path, r.body)
		}
	}
	if strings.Contains(h.out.String(), testAuthKey) || strings.Contains(h.err.String(), testAuthKey) {
		t.Fatalf("the auth key was printed: stdout %q stderr %q", h.out, h.err)
	}
}

func TestForkPauseSendsPauseAndResumeAndPrintsTheOperation(t *testing.T) {
	f := newFakeFork(t, false, false)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("fork", "demo", "--name", "demo-2", "--pause"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	post := f.find("POST", "/sandbox/sbx_1/fork")
	if post == nil || post.body != `{"name":"demo-2","sourcePolicy":"pause_and_resume"}` {
		t.Fatalf("fork %+v", post)
	}
	if len(f.keys) != 1 || f.keys[0] == "" {
		t.Fatalf("Idempotency-Key %v", f.keys)
	}
	stderr := h.err.String()
	if !strings.Contains(stderr, "demo-2 (sbx_2) accepted, operation fork_1") || !strings.Contains(stderr, "Ctrl-C stops only this wait") || !strings.Contains(stderr, "zipbox sandbox cancel demo-2") {
		t.Fatalf("stderr %q", stderr)
	}
	if !strings.Contains(h.out.String(), "sbx_2") || !strings.Contains(h.out.String(), "ready   true") {
		t.Fatalf("stdout %q", h.out)
	}
}

func TestForkWithoutPauseOnARunningSourceHintsAtPause(t *testing.T) {
	f := newFakeFork(t, false, false)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("fork", "demo", "--name", "demo-2"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	post := f.find("POST", "/sandbox/sbx_1/fork")
	if post == nil || post.body != `{"name":"demo-2","sourcePolicy":"require_paused"}` {
		t.Fatalf("fork %+v", post)
	}
	if n := len(f.keys); n != 1 {
		t.Fatalf("pause_required was retried: %d POSTs", n)
	}
	stderr := h.err.String()
	if !strings.Contains(stderr, "Re-run with --pause") || !strings.Contains(stderr, "(pause_required)") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestForkReadsTheTailscaleKeyFromAFileOrStdin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tskey.txt")
	if err := os.WriteFile(file, []byte(testAuthKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stdin string
		path  string
	}{{"", file}, {testAuthKey + "\n", "-"}} {
		f := newFakeFork(t, true, false)
		h := newHarness(t, f.server.URL, tc.stdin)
		h.env["ZIPBOX_API_KEY"] = testKey
		if code := h.run("fork", "demo", "--name", "demo-2", "--pause", "--private", "--tailscale-auth-key-file", tc.path); code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.path, code, h.err)
		}
		post := f.find("POST", "/sandbox/sbx_1/fork")
		if post == nil || post.body != `{"name":"demo-2","sourcePolicy":"pause_and_resume","private":true}` {
			t.Fatalf("%s: fork %+v", tc.path, post)
		}
		put := f.find("PUT", "/sandbox/sbx_2/tailscale")
		if put == nil {
			t.Fatalf("%s: the key was not sent to the new box: %v", tc.path, f.requests())
		}
		if got := openEnvelope(t, f.priv, put.body, "sbx_2"); got != testAuthKey {
			t.Fatalf("%s: the new box opened %q", tc.path, got)
		}
		f.assertKeyNeverInClear(t, h)
	}
}

func TestForkNeverTakesTheKeyFromArgv(t *testing.T) {
	for _, args := range [][]string{
		{"fork", "demo", "--name", "demo-2", "--tailscale-auth-key", testAuthKey},
		{"fork", "demo", "--name", "demo-2", "--tailscale-auth-key=" + testAuthKey},
		{"fork", "demo", "--name", "demo-2", "--auth-key", testAuthKey},
		{"fork", "demo", testAuthKey, "--name", "demo-2"},
	} {
		f := newFakeFork(t, true, false)
		h := newHarness(t, f.server.URL, "")
		h.env["ZIPBOX_API_KEY"] = testKey
		if code := h.run(args...); code == 0 {
			t.Fatalf("%v: accepted a key on argv", args)
		}
		if len(f.requests()) != 0 {
			t.Fatalf("%v: sent %v", args, f.requests())
		}
		f.assertKeyNeverInClear(t, h)
	}
	// The key's only way in is a file or stdin: no flag of `fork` takes a value but
	// the name and the key's file path.
	h := newHarness(t, "http://127.0.0.1:1", "")
	flags := h.app.forkCommand().Flags()
	checked := 0
	for _, field := range strings.Fields(flags.FlagUsages()) {
		name := strings.TrimPrefix(field, "--")
		flag := flags.Lookup(name)
		if name == field || flag == nil {
			continue
		}
		checked++
		if flag.Value.Type() != "bool" && name != "name" && name != "tailscale-auth-key-file" {
			t.Errorf("flag --%s takes a value", name)
		}
	}
	if checked != 5 {
		t.Fatalf("checked %d flags, want --name, --pause, --private, --tailscale-auth-key-file, --no-wait", checked)
	}
}

func TestForkNoWaitWithAKeyIsAUsageError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tskey.txt")
	if err := os.WriteFile(file, []byte(testAuthKey), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeFork(t, true, false)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("fork", "demo", "--name", "demo-2", "--pause", "--no-wait", "--tailscale-auth-key-file", file); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if len(f.requests()) != 0 || !strings.Contains(h.err.String(), "needs the wait") {
		t.Fatalf("requests %v stderr %q", f.requests(), h.err)
	}
	f.assertKeyNeverInClear(t, h)
}

func TestForkJSON(t *testing.T) {
	f := newFakeFork(t, false, false)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("fork", "demo", "--name", "demo-2", "--pause", "--no-wait", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	var accepted map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &accepted); err != nil {
		t.Fatalf("--no-wait --json: %v: %s", err, h.out)
	}
	fork, _ := accepted["fork"].(map[string]any)
	if accepted["id"] != "sbx_2" || accepted["status"] != "provisioning" || accepted["ready"] != false || fork["operationId"] != "fork_1" || fork["phase"] != "queued" {
		t.Fatalf("--no-wait --json %v", accepted)
	}
	if f.find("GET", "/sandbox/sbx_2") != nil || f.find("GET", "/sandbox/sbx_2/wait") != nil {
		t.Fatal("--no-wait waited")
	}

	h.out.Reset()
	if code := h.run("fork", "demo", "--name", "demo-2", "--pause", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	var ready map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &ready); err != nil {
		t.Fatalf("--json: %v: %s", err, h.out)
	}
	if ready["id"] != "sbx_2" || ready["ready"] != true || strings.Contains(h.out.String(), "bt_cli") {
		t.Fatalf("--json %s", h.out)
	}
}

func TestForkReportsWhyTheForkEnded(t *testing.T) {
	f := newFakeFork(t, false, true)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("fork", "demo", "--name", "demo-2", "--pause"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	stderr := h.err.String()
	if !strings.Contains(stderr, "(copy_failed)") || !strings.Contains(stderr, "https://zipbox.ai/docs/errors#copy_failed") {
		t.Fatalf("stderr %q", stderr)
	}
}

// fakeLifecycle serves one box, demo (sbx_1), whose Pause and Resume answer the
// final state, and records each lifecycle request.
func fakeLifecycle(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /sandbox":
			io.WriteString(w, `[{"id":"sbx_1","name":"demo","status":"running","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
			return
		case "POST /sandbox/sbx_1/pause":
			io.WriteString(w, `{"id":"sbx_1","name":"demo","status":"paused","phase":"paused","readiness":"paused","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":false}`)
		case "POST /sandbox/sbx_1/resume":
			fmt.Fprintf(w, `{"id":"sbx_1","name":"demo","status":"running","phase":"running","readiness":"ready","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt_cli"}`, server.URL)
		default:
			http.NotFound(w, r)
		}
		mu.Lock()
		sent = append(sent, r.Method+" "+r.URL.Path)
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), sent...)
		sent = nil
		return out
	}
}

// `zipbox sandbox pause` / `resume` wait for the final state and say so; `stop` and
// `start` are aliases that send the same request and print the same thing.
func TestPauseAndResumeWithStopAndStartAliases(t *testing.T) {
	server, sent := fakeLifecycle(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	for _, tc := range []struct {
		args    []string
		request string
		output  string
		action  string
		status  string
	}{
		{[]string{"sandbox", "pause", "demo"}, "POST /sandbox/sbx_1/pause", "sbx_1 paused.\n", "pause", "paused"},
		{[]string{"sandbox", "stop", "demo"}, "POST /sandbox/sbx_1/pause", "sbx_1 paused.\n", "pause", "paused"},
		{[]string{"sandbox", "resume", "demo"}, "POST /sandbox/sbx_1/resume", "sbx_1 resumed.\n", "resume", "running"},
		{[]string{"sandbox", "start", "demo"}, "POST /sandbox/sbx_1/resume", "sbx_1 resumed.\n", "resume", "running"},
	} {
		h.out.Reset()
		if code := h.run(tc.args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, h.err)
		}
		if got := sent(); len(got) != 1 || got[0] != tc.request {
			t.Fatalf("%v: sent %v, want [%s]", tc.args, got, tc.request)
		}
		if h.out.String() != tc.output {
			t.Fatalf("%v: printed %q, want %q", tc.args, h.out, tc.output)
		}
		h.out.Reset()
		if code := h.run(append(tc.args, "--json")...); code != 0 {
			t.Fatalf("%v --json: exit %d: %s", tc.args, code, h.err)
		}
		sent()
		var out map[string]string
		if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
			t.Fatalf("%v --json: %v: %s", tc.args, err, h.out)
		}
		if out["id"] != "sbx_1" || out["action"] != tc.action || out["status"] != tc.status {
			t.Fatalf("%v --json %v", tc.args, out)
		}
	}
}
