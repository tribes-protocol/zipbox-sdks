package zipbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The shared SDK conformance suite (sdks/conformance/README.md): every fixture
// is served by a mock that checks each request against the next exchange.

type fixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Call        struct {
		Op   string         `json:"op"`
		Args map[string]any `json:"args"`
	} `json:"call"`
	Exchanges []exchange `json:"exchanges"`
	Expect    struct {
		Result json.RawMessage `json:"result"`
		Error  map[string]any  `json:"error"`
	} `json:"expect"`
}

type exchange struct {
	Request struct {
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		Headers  map[string]string `json:"headers"`
		Body     json.RawMessage   `json:"body"`
		BodyText *string           `json:"bodyText"`
	} `json:"request"`
	Response struct {
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		Body     json.RawMessage   `json:"body"`
		BodyText *string           `json:"bodyText"`
	} `json:"response"`
}

const testAPIKey = "zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT"

func fixturesDir(t *testing.T) string {
	for _, dir := range []string{os.Getenv("ZIPBOX_CONFORMANCE_DIR"), "testdata/conformance", "../conformance/fixtures"} {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	t.Fatal("conformance fixtures not found (sdks/conformance/fixtures, or testdata/conformance in the zipbox-go mirror)")
	return ""
}

type mockServer struct {
	t         *testing.T
	mu        sync.Mutex
	exchanges []exchange
	next      int
	failures  []string
}

func (m *mockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.next >= len(m.exchanges) {
		m.failures = append(m.failures, fmt.Sprintf("unexpected request %s %s (all exchanges served)", r.Method, r.RequestURI))
		m.mu.Unlock()
		http.Error(w, "unexpected", 500)
		return
	}
	ex := m.exchanges[m.next]
	m.next++
	m.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if problem := matchRequest(ex, r, body); problem != "" {
		m.mu.Lock()
		m.failures = append(m.failures, fmt.Sprintf("exchange %d: %s", m.next-1, problem))
		m.mu.Unlock()
		http.Error(w, problem, 500)
		return
	}
	for k, v := range ex.Response.Headers {
		w.Header().Set(k, v)
	}
	if ex.Response.BodyText != nil {
		w.WriteHeader(ex.Response.Status)
		_, _ = io.WriteString(w, *ex.Response.BodyText)
		return
	}
	if len(ex.Response.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(ex.Response.Status)
	_, _ = w.Write(ex.Response.Body)
}

func matchRequest(ex exchange, r *http.Request, body []byte) string {
	want := ex.Request
	if r.Method != want.Method {
		return fmt.Sprintf("method %s, want %s", r.Method, want.Method)
	}
	if r.RequestURI != want.Path {
		return fmt.Sprintf("path %s, want %s", r.RequestURI, want.Path)
	}
	for name, value := range want.Headers {
		got := r.Header.Get(name)
		if value == "{{ANY}}" {
			if got == "" {
				return fmt.Sprintf("header %s missing", name)
			}
			continue
		}
		if got != value {
			return fmt.Sprintf("header %s = %q, want %q", name, got, value)
		}
	}
	if len(want.Body) > 0 {
		var gotBody, wantBody any
		if err := json.Unmarshal(body, &gotBody); err != nil {
			return fmt.Sprintf("body is not JSON: %q", body)
		}
		_ = json.Unmarshal(want.Body, &wantBody)
		if !reflect.DeepEqual(gotBody, wantBody) {
			return fmt.Sprintf("body %s, want %s", body, want.Body)
		}
	}
	if want.BodyText != nil && string(body) != *want.BodyText {
		return fmt.Sprintf("body %q, want %q", body, *want.BodyText)
	}
	return ""
}

func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// invoke maps a fixture op onto the Go SDK and returns its result as JSON-ready data.
func invoke(ctx context.Context, c *Client, op string, args map[string]any) (any, error) {
	id := str(args, "id")
	box := c.Sandbox(id)
	switch op {
	case "sandboxes.list":
		return c.Sandboxes.List(ctx)
	case "sandboxes.get":
		return c.Sandboxes.Get(ctx, id)
	case "sandboxes.create":
		b, err := c.Sandboxes.Create(ctx, CreateOptions{Name: str(args, "name"), Tier: str(args, "tier"), SSHPublicKey: str(args, "sshPublicKey")})
		if err != nil {
			return nil, err
		}
		return b.Detail, nil
	case "sandboxes.start":
		return nil, c.Sandboxes.Start(ctx, id)
	case "sandboxes.stop":
		return nil, c.Sandboxes.Stop(ctx, id)
	case "sandboxes.restart":
		return nil, c.Sandboxes.Restart(ctx, id)
	case "sandboxes.setTier":
		return nil, c.Sandboxes.SetTier(ctx, id, str(args, "tier"))
	case "sandboxes.delete":
		return nil, c.Sandboxes.Delete(ctx, id)
	case "sandbox.exec":
		return box.Exec(ctx, str(args, "cmd"))
	case "sandbox.execBackground":
		return box.ExecBackground(ctx, str(args, "cmd"))
	case "commands.events":
		stream, err := box.Commands.Events(ctx, str(args, "commandId"), str(args, "since"))
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		var events []ExecEvent
		for stream.Next() {
			events = append(events, stream.Event())
		}
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return map[string]any{"events": events}, nil
	case "commands.signal":
		return nil, box.Commands.Signal(ctx, str(args, "commandId"), Signal(str(args, "signal")))
	case "files.read":
		data, err := box.Files.Read(ctx, str(args, "path"))
		if err != nil {
			return nil, err
		}
		return map[string]string{"text": string(data)}, nil
	case "files.write":
		return nil, box.Files.Write(ctx, str(args, "path"), strings.NewReader(str(args, "content")))
	case "files.list":
		return box.Files.List(ctx, str(args, "path"))
	case "files.stat":
		return box.Files.Stat(ctx, str(args, "path"))
	case "files.mkdir":
		return nil, box.Files.Mkdir(ctx, str(args, "path"))
	case "files.move":
		return nil, box.Files.Move(ctx, str(args, "path"), str(args, "destination"))
	case "files.delete":
		return nil, box.Files.Delete(ctx, str(args, "path"))
	case "preview.create":
		port, _ := args["port"].(float64)
		return box.Preview.Create(ctx, int(port), str(args, "label"))
	case "account.balance":
		return c.Account.Balance(ctx)
	case "account.whoami":
		return c.Account.WhoAmI(ctx)
	}
	return nil, fmt.Errorf("unknown op %s", op)
}

// subset reports where actual departs from the expected subset.
func subset(path string, expected, actual any) string {
	switch exp := expected.(type) {
	case map[string]any:
		if length, ok := exp["length"]; ok {
			list, isList := actual.([]any)
			if !isList {
				return fmt.Sprintf("%s: want a list, got %T", path, actual)
			}
			if int(length.(float64)) != len(list) {
				return fmt.Sprintf("%s: length %d, want %v", path, len(list), length)
			}
			items, _ := exp["items"].([]any)
			for i, item := range items {
				if p := subset(fmt.Sprintf("%s[%d]", path, i), item, list[i]); p != "" {
					return p
				}
			}
			return ""
		}
		act, ok := actual.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want an object, got %T (%v)", path, actual, actual)
		}
		keys := make([]string, 0, len(exp))
		for k := range exp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p := subset(path+"."+k, exp[k], act[k]); p != "" {
				return p
			}
		}
		return ""
	case []any:
		act, ok := actual.([]any)
		if !ok || len(act) != len(exp) {
			return fmt.Sprintf("%s: %v, want %v", path, actual, expected)
		}
		for i := range exp {
			if p := subset(fmt.Sprintf("%s[%d]", path, i), exp[i], act[i]); p != "" {
				return p
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(expected, actual) {
			return fmt.Sprintf("%s: %v, want %v", path, actual, expected)
		}
		return ""
	}
}

func toJSONValue(t *testing.T, v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

func TestConformance(t *testing.T) {
	dir := fixturesDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".json"), func(t *testing.T) {
			mock := &mockServer{t: t}
			server := httptest.NewServer(mock)
			defer server.Close()
			base := server.URL
			u, _ := url.Parse(base)
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.NewReplacer("{{BASE}}", base, "{{HOST}}", u.Hostname()).Replace(string(raw))
			var fx fixture
			if err := json.Unmarshal([]byte(text), &fx); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			mock.exchanges = fx.Exchanges
			c := New(testAPIKey, WithBaseURL(base), WithBackoff(time.Millisecond, 5*time.Millisecond), WithPollInterval(time.Millisecond, 5*time.Millisecond))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, callErr := invoke(ctx, c, fx.Call.Op, fx.Call.Args)

			mock.mu.Lock()
			failures := append([]string(nil), mock.failures...)
			served := mock.next
			mock.mu.Unlock()
			for _, f := range failures {
				t.Errorf("mock: %s", f)
			}
			if served != len(fx.Exchanges) {
				t.Errorf("served %d of %d exchanges", served, len(fx.Exchanges))
			}

			if fx.Expect.Error != nil {
				var zerr *Error
				if !errors.As(callErr, &zerr) {
					t.Fatalf("want a *zipbox.Error, got %v (result %v)", callErr, result)
				}
				got := map[string]any{"code": string(zerr.Code), "message": zerr.Message, "requestId": zerr.RequestID, "docsUrl": zerr.DocsURL}
				if zerr.Status != 0 {
					got["status"] = float64(zerr.Status)
				}
				if p := subset("error", fx.Expect.Error, got); p != "" {
					t.Error(p)
				}
				return
			}
			if callErr != nil {
				t.Fatalf("call failed: %v", callErr)
			}
			var expected any
			_ = json.Unmarshal(fx.Expect.Result, &expected)
			if expected == nil {
				if result != nil && !reflect.ValueOf(result).IsZero() {
					t.Errorf("want no result, got %v", result)
				}
				return
			}
			if p := subset("result", expected, toJSONValue(t, result)); p != "" {
				t.Error(p)
			}
		})
	}
}
