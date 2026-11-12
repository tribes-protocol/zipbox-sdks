package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testKey = "zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT"
	// otherKey is a key of another account, did:privy:u2.
	otherKey = "zb_OOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOOO"
	// testAccount is the account a redeemed code signs in to.
	testAccount = `{"userId":"did:privy:u1","email":"u1@example.com"}`
)

// fakeZipbox serves the control plane and one box from one server.
func fakeZipbox(t *testing.T) *httptest.Server {
	t.Helper()
	return fakeZipboxAccount(t, testAccount)
}

// fakeZipboxAccount is fakeZipbox whose code redemption answers account as
// the redeem answer's "account" (raw JSON; "" leaves it out).
func fakeZipboxAccount(t *testing.T, account string) *httptest.Server {
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
	// whoami knows testKey (did:privy:u1) and otherKey (did:privy:u2).
	mux.HandleFunc("/account/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+otherKey {
			io.WriteString(w, `{"userId":"did:privy:u2","apiKey":{"id":"k2","name":"desk","kind":"cli","last4":"OOOO","createdAt":"2026-10-19T00:00:00Z"}}`)
			return
		}
		auth(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"userId":"did:privy:u1","apiKey":{"id":"k1","name":"laptop","kind":"cli","last4":"TTTT","createdAt":"2026-10-19T00:00:00Z"}}`)
		})(w, r)
	})
	// Code redemption. A real code is opaque; this fake issues the PKCE
	// challenge itself as the code, so a redeem succeeds only when the verifier
	// hashes to it. The code is not a credential, so no Authorization rides along.
	mux.HandleFunc("/account/cli/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code     string `json:"code"`
			Verifier string `json:"verifier"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		sum := sha256.Sum256([]byte(body.Verifier))
		if err != nil || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Authorization") != "" || body.Code != base64.RawURLEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"invalid_code","message":"bad code","requestId":"r","retryable":false}}`)
			return
		}
		reply := fmt.Sprintf(`{"key":{"id":"k1","name":"laptop","kind":"cli","last4":"TTTT","createdAt":"2026-10-19T00:00:00Z"},"secret":%q`, testKey)
		if account != "" {
			reply += `,"account":` + account
		}
		io.WriteString(w, reply+"}")
	})
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

// The listener takes only a GET /callback navigation. A POST with the right
// state, form or JSON, is refused without a redeem from any origin, and a
// preflight gets no CORS allowance: a key never arrives through the listener.
func TestLoginCallbackRefusesAPost(t *testing.T) {
	verifier, challenge := pkcePair()
	srv, base, calls := codeServer(t, verifier)

	pre, _ := http.NewRequest("OPTIONS", base+"/callback", nil)
	pre.Header.Set("Origin", "https://zipbox.ai")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	pre.Header.Set("Access-Control-Request-Private-Network", "true")
	resp, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Access-Control-Allow-Origin") != "" ||
		resp.Header.Get("Access-Control-Allow-Private-Network") != "" {
		t.Fatalf("preflight %d %v", resp.StatusCode, resp.Header)
	}

	bodies := map[string]string{
		"application/x-www-form-urlencoded": url.Values{"state": {"s3cret"}, "code": {challenge}}.Encode(),
		"application/json":                  fmt.Sprintf(`{"state":"s3cret","code":%q}`, challenge),
	}
	for contentType, body := range bodies {
		for _, origin := range []string{"", "https://zipbox.ai", "null"} {
			// The query is valid too: only the method is wrong.
			req, _ := http.NewRequest("POST", base+"/callback?state=s3cret&code="+challenge, strings.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 405 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("%s origin %q: a POST answered %d %v", contentType, origin, resp.StatusCode, resp.Header)
			}
		}
	}
	for _, method := range []string{"PUT", "HEAD"} {
		req, _ := http.NewRequest(method, base+"/callback?state=s3cret&code="+challenge, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Fatalf("%s answered %d", method, resp.StatusCode)
		}
	}
	resp, err = http.Get(base + "/other?state=s3cret&code=" + challenge)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("another path answered %d", resp.StatusCode)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("refused requests redeemed %d times", n)
	}
	select {
	case got := <-srv.results:
		t.Fatalf("a refused request delivered %+v", got)
	default:
	}
}

const (
	authDone   = "https://zipbox.ai/cli/auth/done"
	authFailed = "https://zipbox.ai/cli/auth/done?error=1"
)

// codeServer is a loginServer whose redeem goes to the fake control plane with
// verifier, counting the calls.
func codeServer(t *testing.T, verifier string) (*loginServer, string, *atomic.Int32) {
	t.Helper()
	h := newHarness(t, fakeZipbox(t).URL, "")
	calls := &atomic.Int32{}
	srv := &loginServer{
		state: "s3cret", origin: "https://zipbox.ai", web: "https://zipbox.ai", results: make(chan loginResult, 1),
		redeem: func(ctx context.Context, code string) (signIn, error) {
			calls.Add(1)
			return h.app.redeemCode(ctx, code, verifier)
		},
	}
	server := httptest.NewServer(srv)
	t.Cleanup(server.Close)
	return srv, server.URL, calls
}

// getCallback sends the page's loopback redirect, a top-level GET navigation,
// and returns the status and Location without following the redirect. A
// Location carrying the code or state fails the test.
func getCallback(t *testing.T, base, origin string, query url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/callback?"+query.Encode(), nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	for _, values := range query {
		for _, v := range values {
			if v != "" && strings.Contains(loc, v) {
				t.Fatalf("Location %q carries %q", loc, v)
			}
		}
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Private-Network") != "" {
		t.Fatalf("callback answered with CORS headers %v", resp.Header)
	}
	if resp.StatusCode == 303 && resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("303 without Referrer-Policy: no-referrer: %v", resp.Header)
	}
	return resp.StatusCode, loc
}

func TestPKCEPair(t *testing.T) {
	verifier, challenge := pkcePair()
	sum := sha256.Sum256([]byte(verifier))
	if len(verifier) != 43 || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("verifier %q challenge %q", verifier, challenge)
	}
}

func TestLoginCallbackCode(t *testing.T) {
	verifier, challenge := pkcePair()
	for _, origin := range []string{"", "null", "https://zipbox.ai"} {
		srv, base, calls := codeServer(t, verifier)
		query := url.Values{"state": {"s3cret"}, "code": {challenge}}
		if code, loc := getCallback(t, base, origin, query); code != 303 || loc != authDone {
			t.Fatalf("origin %q: answered %d %q", origin, code, loc)
		}
		if got := <-srv.results; got.in.secret != testKey || got.in.UserID != "did:privy:u1" || got.err != nil {
			t.Fatalf("origin %q: result %+v", origin, got)
		}
		if code, loc := getCallback(t, base, origin, query); code != 303 || loc != authFailed || calls.Load() != 1 {
			t.Fatalf("origin %q: a second callback answered %d %q after %d redeems", origin, code, loc, calls.Load())
		}
		select {
		case <-srv.results:
			t.Fatal("a second callback delivered a result")
		default:
		}
	}
}

func TestLoginCallbackCodeRefusals(t *testing.T) {
	verifier, challenge := pkcePair()
	srv, base, calls := codeServer(t, verifier)
	for name, c := range map[string]struct {
		origin string
		query  url.Values
	}{
		"wrong state":         {"", url.Values{"state": {"wrong"}, "code": {challenge}}},
		"no state":            {"", url.Values{"code": {challenge}}},
		"duplicate state":     {"", url.Values{"state": {"s3cret", "s3cret"}, "code": {challenge}}},
		"duplicate code":      {"", url.Values{"state": {"s3cret"}, "code": {challenge, challenge}}},
		"foreign origin":      {"https://evil.example", url.Values{"state": {"s3cret"}, "code": {challenge}}},
		"key instead of code": {"", url.Values{"state": {"s3cret"}, "key": {testKey}}},
		"key beside code":     {"null", url.Values{"state": {"s3cret"}, "code": {challenge}, "key": {testKey}}},
		"empty key":           {"", url.Values{"state": {"s3cret"}, "code": {challenge}, "key": {""}}},
		"bad code shape":      {"", url.Values{"state": {"s3cret"}, "code": {"short"}}},
	} {
		if code, loc := getCallback(t, base, c.origin, c.query); code != 303 || loc != authFailed {
			t.Fatalf("%s: answered %d %q", name, code, loc)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("refused requests redeemed %d times", n)
	}
	select {
	case <-srv.results:
		t.Fatal("a refused request delivered a result")
	default:
	}
	// None of those spent the single use.
	if code, loc := getCallback(t, base, "", url.Values{"state": {"s3cret"}, "code": {challenge}}); code != 303 || loc != authDone {
		t.Fatalf("valid callback answered %d %q", code, loc)
	}
	if got := <-srv.results; got.in.secret != testKey {
		t.Fatalf("result %+v", got)
	}
}

func TestLoginCallbackCodeRedeemRefused(t *testing.T) {
	verifier, _ := pkcePair()
	_, unknown := pkcePair() // well-formed, but not a code the control plane issued
	srv, base, calls := codeServer(t, verifier)
	if code, loc := getCallback(t, base, "", url.Values{"state": {"s3cret"}, "code": {unknown}}); code != 303 || loc != authFailed {
		t.Fatalf("refused redeem answered %d %q", code, loc)
	}
	got := <-srv.results
	if calls.Load() != 1 || got.in.secret != "" || got.err == nil || !strings.Contains(got.err.Error(), "refused") {
		t.Fatalf("result %+v after %d redeems", got, calls.Load())
	}
}

// The "browser" is the cli/auth page: it hands back a code by a loopback
// redirect, a top-level GET navigation with no Origin. code picks the code
// from the URL's challenge; the challenge itself is what the fake control
// plane redeems for the verifier.
func codeBrowser(t *testing.T, code func(challenge string) string) func(string) error {
	return func(target string) error {
		go func() {
			query, _ := url.ParseQuery(strings.SplitN(target, "?", 2)[1])
			if query.Has("delivery") || len(query.Get("challenge")) != 43 {
				t.Errorf("login URL %q", target)
			}
			callback := url.Values{"state": {query.Get("state")}, "code": {code(query.Get("challenge"))}}
			req, _ := http.NewRequest("GET", "http://127.0.0.1:"+query.Get("port")+"/callback?"+callback.Encode(), nil)
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
}

func TestBrowserLoginEndToEnd(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL+"/", "")
	h.env["ZIPBOX_WEB_URL"] = "https://zipbox.ai"
	browser := codeBrowser(t, func(challenge string) string { return challenge })
	// Before the page answers, someone who read the port and state tries to
	// deliver their own key, by POST and by GET: both are refused without
	// spending the single use, and the real code still lands.
	h.app.browser = func(target string) error {
		query, _ := url.ParseQuery(strings.SplitN(target, "?", 2)[1])
		callback := "http://127.0.0.1:" + query.Get("port") + "/callback"
		body := fmt.Sprintf(`{"state":%q,"key":%q}`, query.Get("state"), "zb_"+strings.Repeat("A", 43))
		resp, err := http.Post(callback, "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != 405 {
			t.Errorf("a POSTed key answered %v %v", resp, err)
		}
		if resp != nil {
			resp.Body.Close()
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		forged := url.Values{"state": {query.Get("state")}, "key": {"zb_" + strings.Repeat("A", 43)}}
		resp, err = client.Get(callback + "?" + forged.Encode())
		if err != nil || resp.StatusCode != 303 || resp.Header.Get("Location") != authFailed {
			t.Errorf("a key in the query answered %v %v", resp, err)
		}
		if resp != nil {
			resp.Body.Close()
		}
		return browser(target)
	}
	if code := h.run("login"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if !strings.Contains(h.err.String(), "https://zipbox.ai/cli/auth?port=") || h.out.String() != "Signed in as u1@example.com with key laptop (…TTTT).\n" {
		t.Fatalf("stderr %q stdout %q", h.err, h.out)
	}
	if cfg, err := h.app.loadConfig(); err != nil || cfg != (config{APIKey: testKey, UserID: "did:privy:u1", Email: "u1@example.com"}) {
		t.Fatalf("saved %+v %v", cfg, err)
	}
}

func TestBrowserLoginFailsWhenTheCodeIsRefused(t *testing.T) {
	server := fakeZipbox(t)
	h := newHarness(t, server.URL, "")
	h.env["ZIPBOX_WEB_URL"] = "https://zipbox.ai"
	h.app.browser = codeBrowser(t, func(string) string { _, other := pkcePair(); return other })
	if code := h.run("login"); code != 1 || !strings.Contains(h.err.String(), "zipbox login again") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if _, err := h.app.loadConfig(); err == nil {
		t.Fatal("a refused login saved a key")
	}
}

// accountHarness is a browser login against a control plane whose redeem
// answers account, with saved (when not nil) already in the config file.
func accountHarness(t *testing.T, account string, saved *config) *harness {
	t.Helper()
	h := newHarness(t, fakeZipboxAccount(t, account).URL, "")
	h.env["ZIPBOX_WEB_URL"] = "https://zipbox.ai"
	h.app.browser = codeBrowser(t, func(challenge string) string { return challenge })
	if saved != nil {
		if err := h.app.saveConfig(*saved); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// assertNoKeyPrinted fails when either key reached stdout or stderr.
func assertNoKeyPrinted(t *testing.T, h *harness) {
	t.Helper()
	for _, key := range []string{testKey, otherKey} {
		if strings.Contains(h.out.String(), key) || strings.Contains(h.err.String(), key) {
			t.Fatalf("a key was printed: stdout %q stderr %q", h.out, h.err)
		}
	}
}

func TestLoginNamesTheAccountByUserIDWhenItHasNoEmail(t *testing.T) {
	h := accountHarness(t, `{"userId":"did:privy:u1","email":null}`, nil)
	if code := h.run("login"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if h.out.String() != "Signed in as did:privy:u1 with key laptop (…TTTT).\n" {
		t.Fatalf("stdout %q", h.out)
	}
	if cfg, err := h.app.loadConfig(); err != nil || cfg != (config{APIKey: testKey, UserID: "did:privy:u1"}) {
		t.Fatalf("saved %+v %v", cfg, err)
	}
	assertNoKeyPrinted(t, h)
}

// The API always names the account a code signed in to: a reply that does not
// is refused and saves nothing.
func TestLoginRefusesARedeemWithoutAnAccount(t *testing.T) {
	for _, account := range []string{"", "null", "{}", `{"userId":"","email":"u1@example.com"}`, `{"email":"u1@example.com"}`} {
		h := accountHarness(t, account, nil)
		if code := h.run("login"); code != 1 || !strings.Contains(h.err.String(), "named no account") {
			t.Fatalf("account %s: exit %d: %s", account, code, h.err)
		}
		if _, err := h.app.loadConfig(); err == nil {
			t.Fatalf("account %s: saved a key", account)
		}
	}
}

// A login never replaces the saved key of another account without --force:
// a code injected from someone else's approval would otherwise switch this
// CLI to their account silently. The saved account comes from the config, or
// for a config saved before it was recorded, from whoami with the saved key.
func TestLoginRefusesToSwitchAccountsWithoutForce(t *testing.T) {
	for name, c := range map[string]struct {
		saved config
		want  string
	}{
		"saved userId": {
			config{APIKey: otherKey, UserID: "did:privy:u2", Email: "u2@example.com"},
			"this CLI is signed in to u2@example.com, but the approval came from u1@example.com; if you meant to switch accounts, run zipbox login --force",
		},
		"old config, whoami": {
			config{APIKey: otherKey},
			"this CLI is signed in to did:privy:u2, but the approval came from u1@example.com; if you meant to switch accounts, run zipbox login --force",
		},
	} {
		h := accountHarness(t, testAccount, &c.saved)
		if code := h.run("login"); code != 1 || !strings.Contains(h.err.String(), "error: "+c.want+"\n") {
			t.Fatalf("%s: exit %d: %q", name, code, h.err)
		}
		if cfg, err := h.app.loadConfig(); err != nil || cfg != c.saved {
			t.Fatalf("%s: a refused login changed the config: %+v %v", name, cfg, err)
		}
		assertNoKeyPrinted(t, h)
		h.err.Reset()
		if code := h.run("login", "--force"); code != 0 || h.out.String() != "Signed in as u1@example.com with key laptop (…TTTT).\n" {
			t.Fatalf("%s: --force exit %d: %q %q", name, code, h.out, h.err)
		}
		if cfg, err := h.app.loadConfig(); err != nil || cfg != (config{APIKey: testKey, UserID: "did:privy:u1", Email: "u1@example.com"}) {
			t.Fatalf("%s: --force saved %+v %v", name, cfg, err)
		}
		assertNoKeyPrinted(t, h)
	}
}

// A re-login to the same account replaces the key silently, and so does a
// login over a saved key that no longer answers whoami (no saved account).
func TestLoginReplacesTheSameAccountOrADeadKey(t *testing.T) {
	for name, saved := range map[string]config{
		"saved userId":       {APIKey: otherKey, UserID: "did:privy:u1", Email: "old@example.com"},
		"old config, whoami": {APIKey: testKey},
		"whoami fails":       {APIKey: "zb_" + strings.Repeat("X", 43)},
	} {
		h := accountHarness(t, testAccount, &saved)
		if code := h.run("login"); code != 0 || !strings.Contains(h.out.String(), "Signed in as u1@example.com") {
			t.Fatalf("%s: exit %d: %q %q", name, code, h.out, h.err)
		}
		if cfg, err := h.app.loadConfig(); err != nil || cfg != (config{APIKey: testKey, UserID: "did:privy:u1", Email: "u1@example.com"}) {
			t.Fatalf("%s: saved %+v %v", name, cfg, err)
		}
	}
}

// --with-key names the key's account by whoami and makes the same check.
func TestLoginWithKeyChecksTheAccount(t *testing.T) {
	server := fakeZipbox(t)
	for name, saved := range map[string]config{
		"saved userId":       {APIKey: otherKey, UserID: "did:privy:u2", Email: "u2@example.com"},
		"old config, whoami": {APIKey: otherKey},
	} {
		h := newHarness(t, server.URL, testKey+"\n")
		if err := h.app.saveConfig(saved); err != nil {
			t.Fatal(err)
		}
		savedName := "did:privy:u2"
		if saved.Email != "" {
			savedName = saved.Email
		}
		want := "this CLI is signed in to " + savedName +
			", but that key belongs to did:privy:u1; if you meant to switch accounts, run zipbox login --with-key --force"
		if code := h.run("login", "--with-key"); code != 1 || !strings.Contains(h.err.String(), "error: "+want+"\n") {
			t.Fatalf("%s: exit %d: %q", name, code, h.err)
		}
		if cfg, err := h.app.loadConfig(); err != nil || cfg != saved {
			t.Fatalf("%s: a refused login changed the config: %+v %v", name, cfg, err)
		}
		h.app.in = strings.NewReader(testKey + "\n")
		if code := h.run("login", "--with-key", "--force"); code != 0 || h.out.String() != "Signed in as did:privy:u1 with key laptop (…TTTT).\n" {
			t.Fatalf("%s: --force exit %d: %q %q", name, code, h.out, h.err)
		}
		if cfg, err := h.app.loadConfig(); err != nil || cfg != (config{APIKey: testKey, UserID: "did:privy:u1"}) {
			t.Fatalf("%s: --force saved %+v %v", name, cfg, err)
		}
		assertNoKeyPrinted(t, h)
	}
	h := newHarness(t, server.URL, testKey+"\n")
	if err := h.app.saveConfig(config{APIKey: testKey, UserID: "did:privy:u1"}); err != nil {
		t.Fatal(err)
	}
	if code := h.run("login", "--with-key"); code != 0 {
		t.Fatalf("same account: exit %d: %s", code, h.err)
	}
}

// The listener answers every malformed request target over HTTP, without a
// redeem and without spending its single use, and keeps serving.
func TestLoginCallbackMalformedTargets(t *testing.T) {
	verifier, challenge := pkcePair()
	srv, base, calls := codeServer(t, verifier)
	for _, target := range []string{
		"/%", "/a|b", "/callback?%", "/callback?a=^", "/callback?state=%zz&code=x",
		"/callback?state=s3cret&code=%zz", "/callback?state=s3cret;code=" + challenge,
		"/callback?state=s3cret&code=" + challenge + "%", "/%00", "*", "http://x/callback", "//callback",
	} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatalf("%q: no HTTP answer: %v", target, err)
		}
		resp.Body.Close()
		if !(resp.StatusCode == 303 && resp.Header.Get("Location") == authFailed) && (resp.StatusCode < 400 || resp.StatusCode > 499) {
			t.Fatalf("%q: answered %d %q", target, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("malformed requests redeemed %d times", n)
	}
	if code, loc := getCallback(t, base, "", url.Values{"state": {"s3cret"}, "code": {challenge}}); code != 303 || loc != authDone {
		t.Fatalf("valid callback answered %d %q", code, loc)
	}
	if got := <-srv.results; got.in.secret != testKey || got.err != nil {
		t.Fatalf("result %+v", got)
	}
}

// pasteHarness runs --no-browser against the fake control plane, counting the
// redeems it receives.
func pasteHarness(t *testing.T, stdin io.Reader) (*harness, *atomic.Int32) {
	t.Helper()
	inner := fakeZipbox(t)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/cli/token" {
			calls.Add(1)
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server.URL, "")
	h.app.in = stdin
	h.env["ZIPBOX_WEB_URL"] = "https://zipbox.ai"
	h.app.browser = func(string) error { t.Error("--no-browser opened a browser"); return nil }
	return h, calls
}

// --no-browser opens no listener: it prints a link carrying only the PKCE
// challenge, reads the code the page shows from stdin, and redeems it with the
// verifier.
func TestPasteLoginEndToEnd(t *testing.T) {
	stdin, feed := io.Pipe()
	h, calls := pasteHarness(t, stdin)
	stderr := &syncBuffer{}
	h.app.err = stderr
	done := make(chan int, 1)
	go func() { done <- h.run("login", "--no-browser") }()
	var target string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if strings.Contains(stderr.String(), "Paste the sign-in code: ") {
			lines := strings.Split(stderr.String(), "\n")
			if lines[0] != "Open this link on any device, approve, then paste the code it shows here." {
				t.Fatalf("stderr %q", stderr)
			}
			target = strings.TrimSpace(lines[1])
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no paste prompt: %q", stderr)
		}
	}
	link, err := url.Parse(target)
	if err != nil || link.Scheme+"://"+link.Host+link.Path != "https://zipbox.ai/cli/auth" {
		t.Fatalf("--no-browser URL %q", target)
	}
	query := link.Query()
	if len(query.Get("challenge")) != 43 || query.Get("paste") != "1" || len(query) != 2 {
		t.Fatalf("--no-browser URL %q", target)
	}
	// The fake issues the challenge as the code, so the redeem succeeds only
	// when the CLI sends the verifier that hashes to it.
	if _, err := io.WriteString(feed, "  "+query.Get("challenge")+"  \n"); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 || !strings.Contains(h.out.String(), "Signed in as u1@example.com") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d redeems", calls.Load())
	}
	if cfg, err := h.app.loadConfig(); err != nil || cfg.APIKey != testKey || cfg.UserID != "did:privy:u1" {
		t.Fatalf("saved %+v %v", cfg, err)
	}
}

func TestPasteLoginRefusesAMalformedCode(t *testing.T) {
	for _, pasted := range []string{"short\n", testKey + "\n", ""} {
		h, calls := pasteHarness(t, strings.NewReader(pasted))
		if code := h.run("login", "--no-browser"); code != 1 || !strings.Contains(h.err.String(), "sign-in code") {
			t.Fatalf("pasted %q: exit %d: %s", pasted, code, h.err)
		}
		if calls.Load() != 0 {
			t.Fatalf("pasted %q: redeemed %d times", pasted, calls.Load())
		}
		if _, err := h.app.loadConfig(); err == nil {
			t.Fatalf("pasted %q: saved a key", pasted)
		}
	}
}

func TestPasteLoginFailsWhenTheCodeIsRefused(t *testing.T) {
	_, other := pkcePair() // well-formed, but not this login's code
	h, calls := pasteHarness(t, strings.NewReader(other+"\n"))
	if code := h.run("login", "--no-browser"); code != 1 || !strings.Contains(h.err.String(), "refused (HTTP 400)") {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d redeems", calls.Load())
	}
	if _, err := h.app.loadConfig(); err == nil {
		t.Fatal("a refused login saved a key")
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
