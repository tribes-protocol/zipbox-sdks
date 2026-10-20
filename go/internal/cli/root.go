// Package cli implements the zipbox command.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

// Version is the CLI version, stamped by goreleaser.
var Version = "dev"

const (
	defaultWebURL = "https://zipbox.ai"
	defaultMCPURL = "https://mcp.zipbox.ai/mcp"
)

// app is everything a command touches, so tests can swap any of it.
type app struct {
	in      io.Reader
	out     io.Writer
	err     io.Writer
	getenv  func(string) string
	http    *http.Client
	browser func(url string) error
	exePath string
	jsonOut bool
	// stdinIsTerminal reports whether in is an interactive terminal.
	stdinIsTerminal func() bool
}

func (a *app) env(key, fallback string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return fallback
}

func (a *app) webURL() string { return strings.TrimRight(a.env("ZIPBOX_WEB_URL", defaultWebURL), "/") }
func (a *app) mcpURL() string { return a.env("ZIPBOX_MCP_URL", defaultMCPURL) }

// apiKey is ZIPBOX_API_KEY, else the key `zipbox login` saved.
func (a *app) apiKey() string {
	if key := a.getenv("ZIPBOX_API_KEY"); key != "" {
		return key
	}
	cfg, _ := a.loadConfig()
	return cfg.APIKey
}

func (a *app) client() (*zipbox.Client, error) {
	key := a.apiKey()
	if key == "" {
		return nil, errNotLoggedIn
	}
	opts := []zipbox.Option{zipbox.WithHTTPClient(a.http)}
	if base := a.getenv("ZIPBOX_API_URL"); base != "" {
		opts = append(opts, zipbox.WithBaseURL(base))
	}
	return zipbox.New(key, opts...), nil
}

var errNotLoggedIn = errors.New("not logged in: run `zipbox login`, or set ZIPBOX_API_KEY")

// exitError ends the process with a status without printing anything more
// (the remote command's own exit status, for exec and run).
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// printJSON writes v as indented JSON: the --json output of every command.
func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emit prints v as JSON under --json, else runs human.
func (a *app) emit(v any, human func()) error {
	if a.jsonOut {
		return a.printJSON(v)
	}
	human()
	return nil
}

// Main runs the CLI and returns the process exit status.
func Main(args []string) int {
	a := &app{
		in:              os.Stdin,
		out:             os.Stdout,
		err:             os.Stderr,
		getenv:          os.Getenv,
		http:            &http.Client{},
		browser:         openBrowser,
		stdinIsTerminal: func() bool { return isTerminal(os.Stdin) },
	}
	if exe, err := os.Executable(); err == nil {
		a.exePath = exe
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return a.run(ctx, args)
}

func (a *app) run(ctx context.Context, args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetIn(a.in)
	root.SetOut(a.out)
	root.SetErr(a.err)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var exit exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	a.printError(err)
	return 1
}

func (a *app) printError(err error) {
	var zerr *zipbox.Error
	if errors.As(err, &zerr) {
		if a.jsonOut {
			enc := json.NewEncoder(a.err)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"error": zerr})
			return
		}
		fmt.Fprintf(a.err, "error: %s (%s)\n  docs: %s\n", zerr.Message, zerr.Code, zerr.DocsURL)
		return
	}
	if a.jsonOut {
		enc := json.NewEncoder(a.err)
		_ = enc.Encode(map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	fmt.Fprintf(a.err, "error: %s\n", err)
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "zipbox",
		Short:         "Drive Zipbox boxes from your terminal",
		Long:          "zipbox creates and manages Zipbox boxes, runs commands, moves files, opens shells,\npublishes ports and serves the Zipbox MCP tools over stdio.\n\nDocs: https://zipbox.ai/docs/cli",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "print machine-readable JSON")
	root.AddCommand(
		a.loginCommand(),
		a.logoutCommand(),
		a.whoamiCommand(),
		a.sandboxCommand(),
		a.execCommand(),
		a.runCommand(),
		a.filesCommand(),
		a.shellCommand(),
		a.sshCommand(),
		a.previewCommand(),
		a.logsCommand(),
		a.docsCommand(),
		a.mcpCommand(),
	)
	return root
}

// resolveBox turns a box id or name into its id.
func resolveBox(ctx context.Context, c *zipbox.Client, ref string) (string, error) {
	boxes, err := c.Sandboxes.List(ctx)
	if err != nil {
		return "", err
	}
	for _, b := range boxes {
		if b.ID == ref {
			return b.ID, nil
		}
	}
	for _, b := range boxes {
		if b.Name != nil && *b.Name == ref && b.Status != zipbox.StatusDestroyed {
			return b.ID, nil
		}
	}
	return "", &zipbox.Error{Code: zipbox.CodeNotFound, Message: "no box with id or name " + ref, DocsURL: "https://zipbox.ai/docs/errors#not_found"}
}

// boxFor is the client and box handle for a command's first argument.
func (a *app) boxFor(ctx context.Context, ref string) (*zipbox.Client, *zipbox.Box, error) {
	c, err := a.client()
	if err != nil {
		return nil, nil, err
	}
	id, err := resolveBox(ctx, c, ref)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Sandbox(id), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
