package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// apiKeyPattern is the shape of an account key: zb_ + 43 base64url chars.
var apiKeyPattern = regexp.MustCompile(`^zb_[A-Za-z0-9_-]{43}$`)

type config struct {
	APIKey string `json:"apiKey,omitempty"`
}

func (a *app) configPath() (string, error) {
	if dir := a.getenv("ZIPBOX_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "zipbox", "config.json"), nil
}

func (a *app) loadConfig() (config, error) {
	var cfg config
	path, err := a.configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

func (a *app) saveConfig(cfg config) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *app) loginCommand() *cobra.Command {
	var withKey, noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in through the browser (or paste a key with --with-key)",
		Long: "Opens zipbox.ai, which creates a CLI key for this machine and hands it back to a\n" +
			"local callback. --with-key reads a key from stdin instead (for CI, set ZIPBOX_API_KEY).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var key string
			var err error
			if withKey {
				key, err = a.readKey()
			} else {
				key, err = a.browserLogin(cmd.Context(), noBrowser)
			}
			if err != nil {
				return err
			}
			if err := a.saveConfig(config{APIKey: key}); err != nil {
				return err
			}
			return a.reportLogin(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&withKey, "with-key", false, "read an API key from stdin")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the login URL instead of opening it")
	return cmd
}

func (a *app) readKey() (string, error) {
	if a.stdinIsTerminal() {
		fmt.Fprint(a.err, "Paste your API key (zb_…): ")
	}
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no key on stdin")
	}
	key := strings.TrimSpace(line)
	if !apiKeyPattern.MatchString(key) {
		return "", errors.New("that is not a Zipbox API key (expected zb_ followed by 43 characters)")
	}
	return key, nil
}

func (a *app) reportLogin(ctx context.Context) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	who, err := c.Account.WhoAmI(ctx)
	if err != nil {
		return err
	}
	return a.emit(who, func() {
		name := ""
		if who.APIKey != nil {
			name = " with key " + who.APIKey.Name + " (…" + who.APIKey.Last4 + ")"
		}
		fmt.Fprintf(a.out, "Logged in as %s%s.\n", who.UserID, name)
	})
}

type callbackBody struct {
	State string `json:"state"`
	Key   string `json:"key"`
}

// loginServer is the localhost callback the zipbox.ai/cli/auth page POSTs the
// minted key to. It accepts exactly one request carrying the expected state.
type loginServer struct {
	state  string
	origin string
	keys   chan string

	mu   sync.Mutex
	used bool
}

func (s *loginServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	// A public page calling a loopback address: Chrome's Private Network Access
	// preflight needs the explicit opt-in header on top of ordinary CORS.
	w.Header().Set("Access-Control-Allow-Origin", s.origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "content-type")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Vary", "Origin")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body callbackBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.State), []byte(s.state)) != 1 {
		http.Error(w, "state mismatch", http.StatusForbidden)
		return
	}
	if !apiKeyPattern.MatchString(body.Key) {
		http.Error(w, "not an API key", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	used := s.used
	s.used = true
	s.mu.Unlock()
	if used {
		http.Error(w, "this login already completed", http.StatusConflict)
		return
	}
	s.keys <- body.Key
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func randomState() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// browserLogin waits for the person to approve in their browser. Waiting on a
// human is the one wait with no deadline; Ctrl-C ends it.
func (a *app) browserLogin(ctx context.Context, noBrowser bool) (string, error) {
	web, err := url.Parse(a.webURL())
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &loginServer{state: randomState(), origin: web.Scheme + "://" + web.Host, keys: make(chan string, 1)}
	server := &http.Server{Handler: srv}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	target := fmt.Sprintf("%s/cli/auth?port=%d&state=%s", a.webURL(), port, srv.state)
	fmt.Fprintf(a.err, "Approve this machine in your browser:\n  %s\n", target)
	if !noBrowser {
		if err := a.browser(target); err != nil {
			fmt.Fprintln(a.err, "(could not open a browser; open the link above)")
		}
	}
	select {
	case key := <-srv.keys:
		return key, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func (a *app) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the saved API key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := a.configPath()
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return a.emit(map[string]bool{"loggedOut": true}, func() {
				fmt.Fprintln(a.out, "Logged out. The key still works until you delete it in Settings → API keys.")
				if a.getenv("ZIPBOX_API_KEY") != "" {
					fmt.Fprintln(a.out, "ZIPBOX_API_KEY is still set in this shell.")
				}
			})
		},
	}
}

func (a *app) whoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the account and key in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.reportLoginAs(cmd.Context())
		},
	}
}

func (a *app) reportLoginAs(ctx context.Context) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	who, err := c.Account.WhoAmI(ctx)
	if err != nil {
		return err
	}
	return a.emit(who, func() {
		fmt.Fprintf(a.out, "user  %s\n", who.UserID)
		if who.APIKey != nil {
			fmt.Fprintf(a.out, "key   %s (%s, …%s)\n", who.APIKey.Name, who.APIKey.Kind, who.APIKey.Last4)
		}
	})
}
