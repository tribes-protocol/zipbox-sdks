package zipbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type envelopeVector struct {
	SandboxID string `json:"sandboxId"`
	Info      string `json:"info"`
	WrongInfo string `json:"wrongInfo"`
	Plaintext string `json:"plaintext"`
	Recipient struct {
		AgentPublicKey     string `json:"agentPublicKey"`
		PrivateKeyPkcs8Pem string `json:"privateKeyPkcs8Pem"`
	} `json:"recipient"`
	Ephemeral struct {
		PrivateKeyHex string `json:"privateKeyHex"`
	} `json:"ephemeral"`
	IV       string      `json:"iv"`
	Envelope boxEnvelope `json:"envelope"`
}

func loadVector(t *testing.T) envelopeVector {
	t.Helper()
	for _, p := range []string{"testdata/vectors/box_envelope.json", "../conformance/vectors/box_envelope.json"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v envelopeVector
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("parse vector: %v", err)
		}
		return v
	}
	t.Fatal("box envelope vector not found (sdks/conformance/vectors, or testdata/vectors in the zipbox-go mirror)")
	return envelopeVector{}
}

// openEnvelope is the box side (apps/microvmd BoxEnvelope.ts), in Go: PKCS8 key,
// ECDH, HKDF-SHA256 with an empty salt, AES-256-GCM with the tag at the end.
func openEnvelope(t *testing.T, env *boxEnvelope, pkcs8Pem, info string) (string, error) {
	t.Helper()
	block, _ := pem.Decode([]byte(pkcs8Pem))
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := key.(*ecdsa.PrivateKey).ECDH()
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := base64.RawURLEncoding.DecodeString(env.EphemeralPublicKey)
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return "", err
	}
	pub, _ := parsed.(*ecdsa.PublicKey).ECDH()
	shared, err := priv.ECDH(pub)
	if err != nil {
		return "", err
	}
	aesKey, _ := hkdf.Key(sha256.New, shared, nil, info, 32)
	c, _ := aes.NewCipher(aesKey)
	gcm, _ := cipher.NewGCM(c)
	iv, _ := base64.RawURLEncoding.DecodeString(env.IV)
	sealed, _ := base64.RawURLEncoding.DecodeString(env.Ciphertext)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	return string(plain), err
}

func TestBoxEnvelopeVectorByteForByte(t *testing.T) {
	v := loadVector(t)
	if tailscaleEnvelopeInfo(v.SandboxID) != v.Info {
		t.Fatalf("info %q, want %q", tailscaleEnvelopeInfo(v.SandboxID), v.Info)
	}
	scalar, _ := hex.DecodeString(v.Ephemeral.PrivateKeyHex)
	ephemeral, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		t.Fatal(err)
	}
	iv, _ := base64.RawURLEncoding.DecodeString(v.IV)
	got, err := sealBoxEnvelopeWith(v.Recipient.AgentPublicKey, v.Info, []byte(v.Plaintext), ephemeral, iv)
	if err != nil {
		t.Fatal(err)
	}
	if *got != v.Envelope {
		t.Fatalf("envelope\n got %+v\nwant %+v", *got, v.Envelope)
	}
}

func TestBoxEnvelopeVectorOpensOnlyUnderItsInfo(t *testing.T) {
	v := loadVector(t)
	plain, err := openEnvelope(t, &v.Envelope, v.Recipient.PrivateKeyPkcs8Pem, v.Info)
	if err != nil || plain != v.Plaintext {
		t.Fatalf("open: %q, %v", plain, err)
	}
	if _, err := openEnvelope(t, &v.Envelope, v.Recipient.PrivateKeyPkcs8Pem, v.WrongInfo); err == nil {
		t.Fatal("opened under another purpose's info")
	}
}

func TestSealBoxEnvelopeRoundTripsWithFreshRandomness(t *testing.T) {
	v := loadVector(t)
	a, err := sealBoxEnvelope(v.Recipient.AgentPublicKey, v.Info, []byte(v.Plaintext))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sealBoxEnvelope(v.Recipient.AgentPublicKey, v.Info, []byte(v.Plaintext))
	if a.EphemeralPublicKey == b.EphemeralPublicKey || a.IV == b.IV {
		t.Fatal("two seals reused the ephemeral key or the iv")
	}
	for _, field := range []string{a.EphemeralPublicKey, a.IV, a.Ciphertext} {
		if strings.ContainsAny(field, "+/=") {
			t.Fatalf("field %q is not unpadded base64url", field)
		}
	}
	if strings.Contains(a.Ciphertext, v.Plaintext) {
		t.Fatal("plaintext in the envelope")
	}
	plain, err := openEnvelope(t, a, v.Recipient.PrivateKeyPkcs8Pem, v.Info)
	if err != nil || plain != v.Plaintext {
		t.Fatalf("open: %q, %v", plain, err)
	}
	// The key is accepted url-safe and unpadded too.
	urlKey := strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(v.Recipient.AgentPublicKey), "=")
	if _, err := sealBoxEnvelope(urlKey, v.Info, []byte("x")); err != nil {
		t.Fatalf("url-safe key: %v", err)
	}
}

func TestSealBoxEnvelopeRejectsABadKeyWithoutEchoingTheSecret(t *testing.T) {
	_, err := sealBoxEnvelope("not-a-key", "zipbox-tailscale-v1|x", []byte("tskey-auth-secret"))
	if err == nil || strings.Contains(err.Error(), "tskey-auth-secret") || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("err %v", err)
	}
}

// privateBox serves the control plane for one private, ready box whose origin
// is boxURL, counting detail reads.
func privateBox(t *testing.T, boxURL string, reads *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox/sbx_1" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(reads, 1)
		fmt.Fprintf(w, `{"id":"sbx_1","name":"build","status":"running","privateMode":true,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt"}`, boxURL)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPrivateBoxOffTailnetFailsFast(t *testing.T) {
	// Nothing listens on port 1: the dial fails, as an unroutable 100.x address does.
	var reads int32
	api := privateBox(t, "http://127.0.0.1:1", &reads)
	c := New(testAPIKey, WithBaseURL(api.URL), WithBackoff(time.Millisecond, 5*time.Millisecond))
	_, err := c.Sandbox("sbx_1").Exec(context.Background(), "true")
	if !IsCode(err, CodeBoxPrivateUnreachable) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(err.Error(), "build is private: it can only be reached from devices on your tailnet") {
		t.Fatalf("message %v", err)
	}
	var zerr *Error
	if !errors.As(err, &zerr) || zerr.Retryable {
		t.Fatalf("want a non-retryable error, got %+v", zerr)
	}
}

func TestPrivateBoxProbeHasADeadline(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	var reads int32
	api := privateBox(t, hang.URL, &reads)
	c := New(testAPIKey, WithBaseURL(api.URL))
	c.privateProbeTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := c.Sandbox("sbx_1").Files.List(context.Background(), "/")
	if !IsCode(err, CodeBoxPrivateUnreachable) {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("probe took %s", time.Since(start))
	}
}

func TestPrivateBoxReachableProbesOnce(t *testing.T) {
	var probes, calls int32
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/challenge":
			atomic.AddInt32(&probes, 1)
			// Any HTTP answer is reachable, even a 404.
			http.NotFound(w, r)
		case "/files":
			atomic.AddInt32(&calls, 1)
			fmt.Fprint(w, `{"entries":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(box.Close)
	var reads int32
	api := privateBox(t, box.URL, &reads)
	c := New(testAPIKey, WithBaseURL(api.URL))
	b := c.Sandbox("sbx_1")
	for i := 0; i < 2; i++ {
		if _, err := b.Files.List(context.Background(), "/"); err != nil {
			t.Fatalf("list %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&probes) != 1 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("probes %d calls %d", atomic.LoadInt32(&probes), atomic.LoadInt32(&calls))
	}
}

func TestSetPrivateForgetsTheCachedBox(t *testing.T) {
	var probes int32
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/challenge" {
			atomic.AddInt32(&probes, 1)
		}
		fmt.Fprint(w, `{"entries":[]}`)
	}))
	t.Cleanup(box.Close)
	private := int32(0)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && r.URL.Path == "/sandbox/sbx_1/private":
			atomic.StoreInt32(&private, 1)
			fmt.Fprint(w, `{"privateMode":true}`)
		case r.URL.Path == "/sandbox/sbx_1":
			fmt.Fprintf(w, `{"id":"sbx_1","status":"running","privateMode":%t,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":true,"baseUrl":%q,"boxToken":"bt"}`, atomic.LoadInt32(&private) == 1, box.URL)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	b := New(testAPIKey, WithBaseURL(api.URL)).Sandbox("sbx_1")
	ctx := context.Background()
	if _, err := b.Files.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPrivate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Files.List(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("a box made private was not probed again: probes %d", n)
	}
}

func TestConnectSendsOnlyCiphertext(t *testing.T) {
	v := loadVector(t)
	var mu sync.Mutex
	var sent []byte
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			fmt.Fprintf(w, `{"id":%q,"status":"running","agentPublicKey":%q,"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z","ready":false}`, v.SandboxID, v.Recipient.AgentPublicKey)
		case r.Method == "PUT":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			sent = body
			mu.Unlock()
			w.WriteHeader(202)
			fmt.Fprint(w, `{"pending":true}`)
		}
	}))
	t.Cleanup(api.Close)
	b := New(testAPIKey, WithBaseURL(api.URL)).Sandbox(v.SandboxID)
	if err := b.Tailscale.Connect(context.Background(), v.Plaintext); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(string(sent), v.Plaintext) || strings.Contains(string(sent), "tskey") {
		t.Fatalf("the auth key left in the clear: %s", sent)
	}
	var body tailscaleConnectBody
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	plain, err := openEnvelope(t, body.Envelope, v.Recipient.PrivateKeyPkcs8Pem, v.Info)
	if err != nil || plain != v.Plaintext || body.AgentPublicKey != v.Recipient.AgentPublicKey {
		t.Fatalf("box could not open what was sent: %q, %v", plain, err)
	}
}

func TestConnectRefusesAnEmptyKey(t *testing.T) {
	err := New(testAPIKey, WithBaseURL("http://127.0.0.1:1")).Sandbox("sbx_1").Tailscale.Connect(context.Background(), "")
	if !IsCode(err, CodeInvalidRequest) {
		t.Fatalf("err %v", err)
	}
}

// getOrCreateServer serves one private box row and a sequence of detail reads
// (the last one repeats), counting the reads.
func getOrCreateServer(t *testing.T, row string, details []string) (*Client, *int) {
	t.Helper()
	reads := 0
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/sandbox", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "["+row+"]")
	})
	mux.HandleFunc("/sandbox/sbx_1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := reads
		reads++
		mu.Unlock()
		if i >= len(details) {
			i = len(details) - 1
		}
		io.WriteString(w, details[i])
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return New("zb_x", WithBaseURL(server.URL), WithPollInterval(time.Millisecond, time.Millisecond)), &reads
}

const privateRow = `{"id":"sbx_1","name":"build","status":"running","privateMode":true,%s"createdAt":"2026-10-19T00:00:00Z","updatedAt":"2026-10-19T00:00:00Z"}`

func TestGetOrCreateReturnsAnUnjoinedPrivateBoxOnceItHasItsKey(t *testing.T) {
	c, reads := getOrCreateServer(t, fmt.Sprintf(privateRow, `"tailscale":null,`), []string{
		`{"id":"sbx_1","status":"running","privateMode":true,"ready":false,"agentPublicKey":null}`,
		`{"id":"sbx_1","status":"running","privateMode":true,"ready":false,"agentPublicKey":"` + loadVector(t).Recipient.AgentPublicKey + `"}`,
	})
	box, err := c.Sandboxes.GetOrCreate(context.Background(), "build", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if box.Detail == nil || box.Detail.Ready || *reads != 2 {
		t.Fatalf("want a not-ready box after 2 reads, got %+v after %d", box.Detail, *reads)
	}
}

func TestGetOrCreateStillWaitsForReadyOnAJoinedPrivateBox(t *testing.T) {
	c, reads := getOrCreateServer(t, fmt.Sprintf(privateRow, `"tailscale":{"ipv4":"100.101.32.7","pending":false},`), []string{
		`{"id":"sbx_1","status":"running","privateMode":true,"tailscale":{"ipv4":"100.101.32.7","pending":false},"ready":false,"agentPublicKey":"` + loadVector(t).Recipient.AgentPublicKey + `"}`,
		`{"id":"sbx_1","status":"running","privateMode":true,"tailscale":{"ipv4":"100.101.32.7","pending":false},"ready":true,"baseUrl":"https://box--build.zbox.sh","boxToken":"bt"}`,
	})
	box, err := c.Sandboxes.GetOrCreate(context.Background(), "build", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if box.Detail == nil || !box.Detail.Ready || *reads != 2 {
		t.Fatalf("want a ready box after 2 reads, got %+v after %d", box.Detail, *reads)
	}
}
