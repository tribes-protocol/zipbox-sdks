package cli

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
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
)

const testAuthKey = "tskey-auth-kCliTest1CNTRL-NotARealKey0123456789"

type recorded struct {
	method, path string
	body         string
}

// fakeTailnet serves the control plane for one box whose agent key is priv.
type fakeTailnet struct {
	server *httptest.Server
	priv   *ecdh.PrivateKey
	mu     sync.Mutex
	reqs   []recorded
}

func newFakeTailnet(t *testing.T) *fakeTailnet {
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
	f := &fakeTailnet{priv: priv}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, string(body)})
		f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /sandbox":
			io.WriteString(w, `[{"id":"sbx_1","name":"demo","status":"running","createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}]`)
		case "POST /sandbox/identity/claim":
			io.WriteString(w, `{"sandboxId":"sbx_1","created":true}`)
		case "GET /sandbox/sbx_1":
			fmt.Fprintf(w, `{"id":"sbx_1","name":"demo","status":"running","privateMode":true,"agentPublicKey":%q,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":false}`, agentKey)
		case "PUT /sandbox/sbx_1/tailscale":
			w.WriteHeader(202)
			io.WriteString(w, `{"pending":true}`)
		case "GET /sandbox/sbx_1/tailscale":
			io.WriteString(w, `{"tailscale":{"state":"connected","tailnet":"example.com","ipv4":"100.101.32.7","ipv6":"fd7a:115c:a1e0::1","dnsName":"demo.tail1234.ts.net","keyExpiry":null,"pending":false},"privateMode":true}`)
		case "DELETE /sandbox/sbx_1/tailscale":
			w.WriteHeader(202)
			io.WriteString(w, `{}`)
		case "PUT /sandbox/sbx_1/private":
			io.WriteString(w, `{"privateMode":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTailnet) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeTailnet) find(method, path string) *recorded {
	for _, r := range f.requests() {
		if r.method == method && r.path == path {
			return &r
		}
	}
	return nil
}

// openSent opens the envelope the CLI sent the way the box does.
func (f *fakeTailnet) openSent(t *testing.T) string {
	t.Helper()
	put := f.find("PUT", "/sandbox/sbx_1/tailscale")
	if put == nil {
		t.Fatal("no PUT /sandbox/sbx_1/tailscale")
	}
	return openEnvelope(t, f.priv, put.body, "sbx_1")
}

// openEnvelope opens a PUT /sandbox/:id/tailscale body with the box key priv, the
// way box sandboxID does.
func openEnvelope(t *testing.T, priv *ecdh.PrivateKey, putBody, sandboxID string) string {
	t.Helper()
	var body struct {
		Envelope struct {
			EphemeralPublicKey string `json:"ephemeralPublicKey"`
			IV                 string `json:"iv"`
			Ciphertext         string `json:"ciphertext"`
		} `json:"envelope"`
		AgentPublicKey string `json:"agentPublicKey"`
	}
	if err := json.Unmarshal([]byte(putBody), &body); err != nil {
		t.Fatal(err)
	}
	spki, _ := base64.RawURLEncoding.DecodeString(body.Envelope.EphemeralPublicKey)
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := parsed.(*ecdsa.PublicKey).ECDH()
	shared, _ := priv.ECDH(pub)
	key, _ := hkdf.Key(sha256.New, shared, nil, "zipbox-tailscale-v1|"+sandboxID, 32)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	iv, _ := base64.RawURLEncoding.DecodeString(body.Envelope.IV)
	sealed, _ := base64.RawURLEncoding.DecodeString(body.Envelope.Ciphertext)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		t.Fatalf("the box could not open the envelope: %v", err)
	}
	return string(plain)
}

func (f *fakeTailnet) assertKeyNeverInClear(t *testing.T, h *harness) {
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

func TestTailscaleConnectReadsTheKeyFromStdin(t *testing.T) {
	f := newFakeTailnet(t)
	h := newHarness(t, f.server.URL, testAuthKey+"\n")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("tailscale", "connect", "demo"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if got := f.openSent(t); got != testAuthKey {
		t.Fatalf("box opened %q", got)
	}
	f.assertKeyNeverInClear(t, h)
}

func TestTailscaleConnectReadsTheKeyFromAFile(t *testing.T) {
	f := newFakeTailnet(t)
	file := filepath.Join(t.TempDir(), "tskey.txt")
	if err := os.WriteFile(file, []byte(testAuthKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("tailscale", "connect", "demo", "--auth-key-file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	if got := f.openSent(t); got != testAuthKey {
		t.Fatalf("box opened %q", got)
	}
	f.assertKeyNeverInClear(t, h)
}

func TestTailscaleConnectNeverTakesTheKeyFromArgv(t *testing.T) {
	for _, args := range [][]string{
		{"tailscale", "connect", "demo", "--auth-key", testAuthKey},
		{"tailscale", "connect", "demo", "--auth-key=" + testAuthKey},
		{"tailscale", "connect", "demo", testAuthKey},
	} {
		f := newFakeTailnet(t)
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
}

func TestTailscaleConnectRefusesSomethingThatIsNotAnAuthKey(t *testing.T) {
	f := newFakeTailnet(t)
	h := newHarness(t, f.server.URL, "zb_notatailscalekey\n")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("tailscale", "connect", "demo"); code == 0 {
		t.Fatal("accepted a non-Tailscale key")
	}
	if len(f.requests()) != 0 || strings.Contains(h.err.String(), "zb_notatailscalekey") {
		t.Fatalf("requests %v stderr %s", f.requests(), h.err)
	}
}

func TestTailscaleStatusAndDisconnect(t *testing.T) {
	f := newFakeTailnet(t)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("tailscale", "status", "demo"); code != 0 {
		t.Fatalf("status exit %d: %s", code, h.err)
	}
	for _, want := range []string{"connected", "example.com", "100.101.32.7", "demo.tail1234.ts.net"} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("status missing %q: %s", want, h.out)
		}
	}
	h.out.Reset()
	if code := h.run("tailscale", "status", "demo", "--json"); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var out struct {
		ID        string         `json:"id"`
		Tailscale map[string]any `json:"tailscale"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil || out.ID != "sbx_1" || out.Tailscale["ipv4"] != "100.101.32.7" {
		t.Fatalf("status --json %v: %s", err, h.out)
	}
	if code := h.run("tailscale", "disconnect", "demo"); code != 0 {
		t.Fatalf("disconnect exit %d: %s", code, h.err)
	}
	if f.find("DELETE", "/sandbox/sbx_1/tailscale") == nil {
		t.Fatal("no DELETE /sandbox/sbx_1/tailscale")
	}
}

func TestPrivateOnOff(t *testing.T) {
	f := newFakeTailnet(t)
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("private", "on", "demo"); code != 0 {
		t.Fatalf("private on exit %d: %s", code, h.err)
	}
	if code := h.run("private", "off", "demo"); code != 0 {
		t.Fatalf("private off exit %d: %s", code, h.err)
	}
	var bodies []string
	for _, r := range f.requests() {
		if r.method == "PUT" && r.path == "/sandbox/sbx_1/private" {
			bodies = append(bodies, r.body)
		}
	}
	if len(bodies) != 2 || bodies[0] != `{"enabled":true}` || bodies[1] != `{"enabled":false}` {
		t.Fatalf("bodies %v", bodies)
	}
	if code := h.run("private", "maybe", "demo"); code == 0 {
		t.Fatal("accepted private maybe")
	}
}

func TestSandboxCreatePrivateJoinsTheTailnet(t *testing.T) {
	f := newFakeTailnet(t)
	file := filepath.Join(t.TempDir(), "tskey.txt")
	if err := os.WriteFile(file, []byte(testAuthKey), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("sandbox", "create", "--name", "demo", "--private", "--tailscale-auth-key-file", file); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err)
	}
	claim := f.find("POST", "/sandbox/identity/claim")
	if claim == nil || claim.body != `{"name":"demo","private":true}` {
		t.Fatalf("claim %+v", claim)
	}
	if got := f.openSent(t); got != testAuthKey {
		t.Fatalf("box opened %q", got)
	}
	f.assertKeyNeverInClear(t, h)
}

func TestSandboxCreateRefusesABadKeyBeforeCreating(t *testing.T) {
	f := newFakeTailnet(t)
	file := filepath.Join(t.TempDir(), "tskey.txt")
	if err := os.WriteFile(file, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, f.server.URL, "")
	h.env["ZIPBOX_API_KEY"] = testKey
	if code := h.run("sandbox", "create", "--private", "--tailscale-auth-key-file", file); code == 0 {
		t.Fatal("created with a bad key")
	}
	if len(f.requests()) != 0 {
		t.Fatalf("sent %v", f.requests())
	}
}
