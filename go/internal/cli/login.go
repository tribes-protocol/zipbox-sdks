package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	zipbox "github.com/tribes-protocol/zipbox-go"
)

// apiKeyPattern is the shape of an account key: zb_ + 43 base64url chars.
var apiKeyPattern = regexp.MustCompile(`^zb_[A-Za-z0-9_-]{43}$`)

// config is the saved key and the account it signs in to. A config saved
// before the account was recorded has only apiKey.
type config struct {
	APIKey string `json:"apiKey,omitempty"`
	UserID string `json:"userId,omitempty"`
	Email  string `json:"email,omitempty"`
}

// signIn is what a login produced: the new key and the account it belongs
// to. It is login's --json output; the secret is never printed.
type signIn struct {
	secret string
	UserID string         `json:"userId"`
	Email  *string        `json:"email"`
	APIKey *zipbox.APIKey `json:"apiKey,omitempty"`
}

// accountName is how the CLI names an account: its email, else its userId.
func accountName(userID, email string) string {
	if email != "" {
		return email
	}
	return userID
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
	var withKey, noBrowser, force bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in through the browser (or paste a key with --with-key)",
		Long: "Opens zipbox.ai, which hands a one-time sign-in code back to a local callback that\n" +
			"redeems it for a CLI key. --no-browser prints a link to approve on any device and\n" +
			"reads the code it shows from stdin. --with-key reads a key from stdin instead (for\n" +
			"CI, set ZIPBOX_API_KEY). A login never replaces the saved key of another account\n" +
			"unless --force is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			var in signIn
			var err error
			again, from := "zipbox login", "the approval came from"
			switch {
			case withKey:
				in, err = a.keyLogin(ctx)
				again, from = "zipbox login --with-key", "that key belongs to"
			case noBrowser:
				in, err = a.pasteLogin(ctx)
				again = "zipbox login --no-browser"
			default:
				in, err = a.browserLogin(ctx)
			}
			if err != nil {
				return err
			}
			newName := accountName(in.UserID, deref(in.Email))
			if saved, savedName := a.savedAccount(ctx); !force && saved != "" && saved != in.UserID {
				return fmt.Errorf("this CLI is signed in to %s, but %s %s; if you meant to switch accounts, run %s --force",
					savedName, from, newName, again)
			}
			if err := a.saveConfig(config{APIKey: in.secret, UserID: in.UserID, Email: deref(in.Email)}); err != nil {
				return err
			}
			return a.emit(in, func() {
				key := ""
				if in.APIKey != nil {
					key = " with key " + in.APIKey.Name + " (…" + in.APIKey.Last4 + ")"
				}
				fmt.Fprintf(a.out, "Signed in as %s%s.\n", newName, key)
			})
		},
	}
	cmd.Flags().BoolVar(&withKey, "with-key", false, "read an API key from stdin")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "approve on any device and paste the code it shows")
	cmd.Flags().BoolVar(&force, "force", false, "replace a saved key that belongs to another account")
	return cmd
}

// savedAccount is the account the saved key signs in to, and its name: the
// userId saved beside the key, else (a config saved before it was recorded)
// whoami with that key. No key, or a key that no longer answers, is none.
func (a *app) savedAccount(ctx context.Context) (userID, name string) {
	cfg, err := a.loadConfig()
	if err != nil || cfg.APIKey == "" {
		return "", ""
	}
	if cfg.UserID != "" {
		return cfg.UserID, accountName(cfg.UserID, cfg.Email)
	}
	who, err := a.clientFor(cfg.APIKey).Account.WhoAmI(ctx)
	if err != nil {
		return "", ""
	}
	return who.UserID, who.UserID
}

// keyLogin is --with-key: the key from stdin, and the account whoami names
// for it.
func (a *app) keyLogin(ctx context.Context) (signIn, error) {
	key, err := a.readKey()
	if err != nil {
		return signIn{}, err
	}
	who, err := a.clientFor(key).Account.WhoAmI(ctx)
	if err != nil {
		return signIn{}, err
	}
	return signIn{secret: key, UserID: who.UserID, APIKey: who.APIKey}, nil
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

// codePattern is the shape of a one-time sign-in code: 43 base64url chars.
var codePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type loginResult struct {
	in  signIn
	err error
}

// loginServer is the localhost callback the zipbox.ai/cli/auth page reports
// to. It accepts exactly one request carrying the expected state.
//
// The page hands the code back by an RFC 8252 §7.3 loopback redirect: a
// top-level GET navigation to http://127.0.0.1:<port>/callback?state=…&code=….
// A top-level navigation is not gated by Chrome's Local Network Access (142+),
// which blocks a public page's fetch() to 127.0.0.1 unless the user grants it,
// and Safari asks nothing for it (it does put up a "non-secure form" prompt
// for a form POST to http). The code in the URL, and so in browser history, is
// safe: it is single use, expires in 5 minutes, and is worthless without the
// PKCE verifier only this process holds. Only a code is accepted, never a key.
//
// PKCE does not say whose approval a code came from. The port, state and
// challenge ride in the browser opener's argv, which another local user can
// read (xdg-open on a shared Linux host); they can approve that challenge from
// their own account and send the code here, and redeeming it would sign this
// CLI in to THEIR account. No URL design closes that, which is why login names
// the account it signed in to and refuses to replace a saved key of another
// account without --force.
type loginServer struct {
	state   string
	origin  string
	web     string
	results chan loginResult
	// redeem trades a code for the account key at the control plane.
	redeem func(ctx context.Context, code string) (signIn, error)

	mu   sync.Mutex
	used bool
}

func (s *loginServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.serveCode(w, r)
}

// serveCode takes the page's loopback redirect of a one-time code and redeems
// it before answering. Every answer is a 303 to the page's done screen, which
// carries neither the code nor the state.
func (s *loginServer) serveCode(w http.ResponseWriter, r *http.Request) {
	refuse := func() { s.seeOther(w, false) }
	// A navigation normally sends no Origin; anything else must be the page.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && origin != s.origin {
		refuse()
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		refuse()
		return
	}
	// A key never travels through the browser: a callback naming one is refused.
	state, code := query["state"], query["code"]
	if len(state) != 1 || len(code) != 1 || query.Has("key") ||
		subtle.ConstantTimeCompare([]byte(state[0]), []byte(s.state)) != 1 ||
		!codePattern.MatchString(code[0]) {
		refuse()
		return
	}
	if !s.claim() {
		refuse()
		return
	}
	in, err := s.redeem(r.Context(), code[0])
	// Answer before handing over the result: browserLogin closes this server
	// as soon as it has one, which would cut the answer off mid-write.
	s.seeOther(w, err == nil)
	s.results <- loginResult{in: in, err: err}
}

func (s *loginServer) seeOther(w http.ResponseWriter, ok bool) {
	target := s.web + "/cli/auth/done"
	if !ok {
		target += "?error=1"
	}
	w.Header().Set("Location", target)
	// The callback URL holds the code: the done page must not get it as Referer.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusSeeOther)
	flush(w)
}

// flush puts a complete answer (it states its Content-Length) on the wire.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// claim spends the listener's single use.
func (s *loginServer) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := s.used
	s.used = true
	return !used
}

func randomState() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// pkcePair is a PKCE verifier and its S256 challenge, both base64url without
// padding (43 chars).
func pkcePair() (verifier, challenge string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *app) apiURL() string {
	return strings.TrimRight(a.env("ZIPBOX_API_URL", zipbox.DefaultBaseURL), "/")
}

// redeemCode trades a sign-in code and its verifier for the account key and
// the account whose approval minted the code. The code alone is not a
// credential, so the request carries no Authorization.
func (a *app) redeemCode(ctx context.Context, code, verifier string) (signIn, error) {
	body, err := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	if err != nil {
		return signIn{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL()+"/account/cli/token", bytes.NewReader(body))
	if err != nil {
		return signIn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return signIn{}, fmt.Errorf("could not redeem the sign-in code: %w; run zipbox login again", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return signIn{}, fmt.Errorf("the sign-in code was refused (HTTP %d); run zipbox login again", resp.StatusCode)
	}
	var out struct {
		Key     *zipbox.APIKey `json:"key"`
		Secret  string         `json:"secret"`
		Account *struct {
			UserID string  `json:"userId"`
			Email  *string `json:"email"`
		} `json:"account"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || !apiKeyPattern.MatchString(out.Secret) {
		return signIn{}, errors.New("the sign-in code was refused: the reply carried no API key; run zipbox login again")
	}
	if out.Account == nil || out.Account.UserID == "" {
		return signIn{}, errors.New("the sign-in code was refused: the reply named no account; run zipbox login again")
	}
	return signIn{secret: out.Secret, UserID: out.Account.UserID, Email: out.Account.Email, APIKey: out.Key}, nil
}

// browserLogin waits for the person to approve in their browser. Waiting on a
// human is the one wait with no deadline; Ctrl-C ends it.
func (a *app) browserLogin(ctx context.Context) (signIn, error) {
	web, err := url.Parse(a.webURL())
	if err != nil {
		return signIn{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return signIn{}, err
	}
	verifier, challenge := pkcePair()
	srv := &loginServer{
		state:   randomState(),
		origin:  web.Scheme + "://" + web.Host,
		web:     a.webURL(),
		results: make(chan loginResult, 1),
		redeem: func(ctx context.Context, code string) (signIn, error) {
			return a.redeemCode(ctx, code, verifier)
		},
	}
	server := &http.Server{Handler: srv}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	target := fmt.Sprintf("%s/cli/auth?port=%d&state=%s&challenge=%s", a.webURL(), port, srv.state, challenge)
	fmt.Fprintf(a.err, "Approve this machine in your browser:\n  %s\n", target)
	if err := a.browser(target); err != nil {
		fmt.Fprintln(a.err, "(could not open a browser; open the link above)")
	}
	select {
	case res := <-srv.results:
		return res.in, res.err
	case <-ctx.Done():
		return signIn{}, ctx.Err()
	}
}

// pasteLogin is --no-browser: usually a remote machine whose loopback the
// browser cannot reach, so there is no listener. The page shows the one-time
// code and the person pastes it here; it is still worthless without the
// verifier only this process holds.
func (a *app) pasteLogin(ctx context.Context) (signIn, error) {
	verifier, challenge := pkcePair()
	fmt.Fprintf(a.err, "Open this link on any device, approve, then paste the code it shows here.\n  %s/cli/auth?challenge=%s&paste=1\n", a.webURL(), challenge)
	fmt.Fprint(a.err, "Paste the sign-in code: ")
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil && line == "" {
		return signIn{}, errors.New("no sign-in code on stdin; run zipbox login --no-browser again")
	}
	code := strings.TrimSpace(line)
	if !codePattern.MatchString(code) {
		return signIn{}, errors.New("that is not a sign-in code (expected the 43 characters the page shows); run zipbox login --no-browser again")
	}
	return a.redeemCode(ctx, code, verifier)
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
