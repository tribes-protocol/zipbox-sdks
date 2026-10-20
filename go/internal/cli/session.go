package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

func (a *app) shellCommand() *cobra.Command {
	var attach string
	cmd := &cobra.Command{
		Use:   "shell <box>",
		Short: "Open an interactive shell on a box (reconnects by itself)",
		Long: "Opens a bash session on the box. The session outlives the connection: if the\n" +
			"network drops it reconnects with backoff and no limit, and --attach re-joins a\n" +
			"session by id from another terminal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.shell(cmd.Context(), box, attach)
		},
	}
	cmd.Flags().StringVar(&attach, "attach", "", "re-join an existing session id")
	return cmd
}

func (a *app) shell(ctx context.Context, box *zipbox.Box, attach string) error {
	stdin, _ := a.in.(*os.File)
	stdout, _ := a.out.(*os.File)
	opts := zipbox.PtyOptions{ClientLabel: "zipbox-cli"}
	if stdout != nil {
		if cols, rows, err := term.GetSize(int(stdout.Fd())); err == nil {
			opts.Cols, opts.Rows = cols, rows
		}
	}
	var pty *zipbox.Pty
	var err error
	if attach != "" {
		pty, err = box.Pty.Attach(ctx, attach, opts)
	} else {
		pty, err = box.Pty.Create(ctx, opts)
	}
	if err != nil {
		return err
	}
	defer pty.Detach()
	fmt.Fprintf(a.err, "[zipbox: session %s — re-attach with: zipbox shell %s --attach %s]\r\n", pty.SessionID, box.ID, pty.SessionID)
	if stdin != nil && term.IsTerminal(int(stdin.Fd())) {
		state, err := term.MakeRaw(int(stdin.Fd()))
		if err == nil {
			defer term.Restore(int(stdin.Fd()), state)
		}
	}
	stopResize := watchResize(stdout, pty)
	defer stopResize()
	go func() { _, _ = io.Copy(pty, a.in) }()
	if _, err := io.Copy(a.out, pty); err != nil && !errors.Is(err, zipbox.ErrPtyClosed) {
		return err
	}
	if code := pty.ExitCode(); code != nil && *code != 0 {
		return exitError{code: *code}
	}
	return nil
}

func (a *app) sshCommand() *cobra.Command {
	var run bool
	cmd := &cobra.Command{
		Use:   "ssh <box>",
		Short: "Print the ssh command for a box (--exec runs it)",
		Long:  "Boxes accept SSH on their IPv6 address with the keys in Settings → SSH keys.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			info, err := box.SSHInfo(cmd.Context())
			if err != nil {
				return err
			}
			if !run {
				return a.emit(info, func() { fmt.Fprintln(a.out, info.Command) })
			}
			ssh := exec.CommandContext(cmd.Context(), "ssh", info.User+"@"+info.Host)
			ssh.Stdin, ssh.Stdout, ssh.Stderr = a.in, a.out, a.err
			if err := ssh.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return exitError{code: exit.ExitCode()}
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&run, "exec", false, "run ssh instead of printing the command")
	return cmd
}

func (a *app) previewCommand() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "preview <box> <port>",
		Short: "Publish a port on a box at a public HTTPS URL",
		Long:  "Previews are public: anyone with the URL reaches the port. Remove one with `zipbox preview rm <box> <label>`.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %q", args[1])
			}
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p, err := box.Preview.Create(cmd.Context(), port, label)
			if err != nil {
				return err
			}
			return a.emit(p, func() { fmt.Fprintln(a.out, p.URL) })
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "subdomain label (default p<port>)")
	cmd.AddCommand(&cobra.Command{
		Use:   "rm <box> <label>",
		Short: "Remove a preview",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := box.Preview.Remove(cmd.Context(), args[1]); err != nil {
				return err
			}
			return a.emit(map[string]bool{"ok": true}, func() {})
		},
	})
	return cmd
}

func (a *app) logsCommand() *cobra.Command {
	var opts zipbox.LogOptions
	cmd := &cobra.Command{
		Use:   "logs <box>",
		Short: "Print a box's system journal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			logs, err := box.Logs(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return a.emit(map[string]string{"logs": logs}, func() { io.WriteString(a.out, logs) })
		},
	}
	cmd.Flags().StringVar(&opts.Since, "since", "", `journalctl time, e.g. "1 hour ago"`)
	cmd.Flags().IntVarP(&opts.Lines, "lines", "n", 0, "last N lines")
	cmd.Flags().StringVarP(&opts.Unit, "unit", "u", "", "one systemd unit")
	return cmd
}

func (a *app) docsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "docs [slug]",
		Short: "Print a docs page as markdown (no slug: the index, llms.txt)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := a.webURL() + "/llms.txt"
			slug := ""
			if len(args) == 1 {
				slug = strings.Trim(strings.TrimSuffix(strings.TrimPrefix(args[0], "/docs/"), ".md"), "/")
				target = a.webURL() + "/docs/" + slug + ".md"
			}
			req, err := http.NewRequestWithContext(cmd.Context(), "GET", target, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Accept", "text/markdown")
			resp, err := a.http.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s: %s", target, resp.Status)
			}
			return a.emit(map[string]string{"slug": slug, "url": target, "markdown": string(body)}, func() {
				a.out.Write(body)
			})
		},
	}
}
