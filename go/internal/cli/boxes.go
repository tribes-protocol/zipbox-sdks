package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

func (a *app) sandboxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"box", "sandboxes"},
		Short:   "Create and manage boxes",
	}
	var create zipbox.CreateOptions
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a box and wait until it is ready",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			fmt.Fprintln(a.err, "Creating box…")
			box, err := c.Sandboxes.Create(cmd.Context(), create)
			if err != nil {
				return err
			}
			return a.emit(box.Detail, func() { a.printDetail(box.Detail) })
		},
	}
	createCmd.Flags().StringVar(&create.Name, "name", "", "the box's permanent subdomain")
	createCmd.Flags().StringVar(&create.Tier, "tier", "", "machine size: xs, s, m, l, xl, xxl")
	createCmd.Flags().StringVar(&create.SSHPublicKey, "ssh-public-key", "", "an OpenSSH public key line to authorize")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your boxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			boxes, err := c.Sandboxes.List(cmd.Context())
			if err != nil {
				return err
			}
			if boxes == nil {
				boxes = []zipbox.Sandbox{}
			}
			return a.emit(boxes, func() {
				w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tNAME\tSTATUS\tTIER")
				for _, b := range boxes {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.ID, deref(b.Name), b.Status, deref(b.Tier))
				}
				w.Flush()
			})
		},
	}

	getCmd := &cobra.Command{
		Use:   "get <box>",
		Short: "Show one box",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			detail, err := box.Refresh(cmd.Context())
			if err != nil {
				return err
			}
			return a.emit(detail, func() { a.printDetail(detail) })
		},
	}

	lifecycle := func(use, short, verb string, run func(ctx context.Context, c *zipbox.Client, id string) error) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <box>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, box, err := a.boxFor(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if err := run(cmd.Context(), c, box.ID); err != nil {
					return err
				}
				return a.emit(map[string]string{"id": box.ID, "action": use}, func() {
					fmt.Fprintf(a.out, "%s %s.\n", box.ID, verb)
				})
			},
		}
	}
	startCmd := lifecycle("start", "Start a stopped box", "starting", func(ctx context.Context, c *zipbox.Client, id string) error {
		return c.Sandboxes.Start(ctx, id)
	})
	stopCmd := lifecycle("stop", "Stop a box (disk archived, nothing billed)", "stopping", func(ctx context.Context, c *zipbox.Client, id string) error {
		return c.Sandboxes.Stop(ctx, id)
	})
	restartCmd := lifecycle("restart", "Hard-restart a box (files survive)", "restarting", func(ctx context.Context, c *zipbox.Client, id string) error {
		return c.Sandboxes.Restart(ctx, id)
	})

	var yes bool
	deleteCmd := &cobra.Command{
		Use:   "delete <box>",
		Short: "Delete a box (a recoverable copy is kept for 30 days)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !yes {
				if !a.stdinIsTerminal() {
					return errors.New("refusing to delete without --yes")
				}
				fmt.Fprintf(a.err, "Delete box %s? Type the id to confirm: ", box.ID)
				line, _ := bufio.NewReader(a.in).ReadString('\n')
				if strings.TrimSpace(line) != box.ID {
					return errors.New("not confirmed")
				}
			}
			if err := c.Sandboxes.Delete(cmd.Context(), box.ID); err != nil {
				return err
			}
			return a.emit(map[string]string{"id": box.ID, "action": "delete"}, func() {
				fmt.Fprintf(a.out, "%s deleted.\n", box.ID)
			})
		},
	}
	deleteCmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")

	tierCmd := &cobra.Command{
		Use:   "tier <box> <tier>",
		Short: "Move a box to another tier",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := c.Sandboxes.SetTier(cmd.Context(), box.ID, args[1]); err != nil {
				return err
			}
			return a.emit(map[string]string{"id": box.ID, "tier": args[1]}, func() {
				fmt.Fprintf(a.out, "%s moving to tier %s.\n", box.ID, args[1])
			})
		},
	}
	cmd.AddCommand(createCmd, listCmd, getCmd, startCmd, stopCmd, restartCmd, deleteCmd, tierCmd)
	return cmd
}

func (a *app) printDetail(d *zipbox.SandboxDetail) {
	w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "id\t%s\n", d.ID)
	fmt.Fprintf(w, "name\t%s\n", deref(d.Name))
	fmt.Fprintf(w, "status\t%s\n", d.Status)
	fmt.Fprintf(w, "tier\t%s\n", deref(d.Tier))
	fmt.Fprintf(w, "ready\t%t\n", d.Ready)
	if d.BaseURL != nil {
		fmt.Fprintf(w, "url\t%s\n", *d.BaseURL)
	}
	if d.IPv6 != nil {
		fmt.Fprintf(w, "ipv6\t%s\n", *d.IPv6)
	}
	w.Flush()
}

type execFlags struct {
	cwd        string
	env        []string
	timeout    time.Duration
	background bool
}

func (f execFlags) options() ([]zipbox.ExecOption, error) {
	var opts []zipbox.ExecOption
	if f.cwd != "" {
		opts = append(opts, zipbox.WithCwd(f.cwd))
	}
	if len(f.env) > 0 {
		env := map[string]string{}
		for _, kv := range f.env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("--env %q: want KEY=VALUE", kv)
			}
			env[k] = v
		}
		opts = append(opts, zipbox.WithEnv(env))
	}
	if f.timeout > 0 {
		opts = append(opts, zipbox.WithTimeout(f.timeout))
	}
	return opts, nil
}

func (a *app) execCommand() *cobra.Command {
	var flags execFlags
	cmd := &cobra.Command{
		Use:   "exec <box> -- <command...>",
		Short: "Run a command on a box; exits with its exit status",
		Long: "Runs the command through `bash -lc` on the box (arguments are joined with spaces,\n" +
			"as ssh does), streams its output, and exits with the remote exit status.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			opts, err := flags.options()
			if err != nil {
				return err
			}
			command := strings.Join(args[1:], " ")
			if flags.background {
				c, err := box.ExecBackground(cmd.Context(), command, opts...)
				if err != nil {
					return err
				}
				return a.emit(c.CommandRecord, func() { fmt.Fprintln(a.out, c.ID) })
			}
			return a.streamExec(cmd.Context(), func(ctx context.Context) (*zipbox.ExecStream, error) {
				return box.ExecStream(ctx, command, opts...)
			})
		},
	}
	cmd.Flags().StringVar(&flags.cwd, "cwd", "", "working directory (default /root/workspace)")
	cmd.Flags().StringArrayVarP(&flags.env, "env", "e", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", 0, "kill the command after this long (default: never)")
	cmd.Flags().BoolVar(&flags.background, "background", false, "start it and print its id; it keeps running")
	return cmd
}

// streamExec prints a command's output as it arrives (or collects it under
// --json) and returns an exitError carrying the remote status.
func (a *app) streamExec(ctx context.Context, open func(ctx context.Context) (*zipbox.ExecStream, error)) error {
	stream, err := open(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	result := zipbox.ExecResult{ExitCode: -1}
	var stdout, stderr strings.Builder
	for stream.Next() {
		ev := stream.Event()
		switch ev.Type {
		case zipbox.EventStart:
			result.ID = ev.ID
		case zipbox.EventStdout:
			if a.jsonOut {
				stdout.WriteString(ev.Data)
			} else {
				io.WriteString(a.out, ev.Data)
			}
		case zipbox.EventStderr:
			if a.jsonOut {
				stderr.WriteString(ev.Data)
			} else {
				io.WriteString(a.err, ev.Data)
			}
		case zipbox.EventGap:
			fmt.Fprintf(a.err, "\n[zipbox: %s]\n", ev.Message)
		case zipbox.EventExit:
			if ev.ExitCode != nil {
				result.ExitCode = *ev.ExitCode
			}
			result.Signal = ev.Signal
			result.TimedOut = ev.TimedOut
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if a.jsonOut {
		result.Stdout, result.Stderr = stdout.String(), stderr.String()
		if err := a.printJSON(result); err != nil {
			return err
		}
	}
	code := result.ExitCode
	if code < 0 {
		code = 1
	}
	if code != 0 {
		return exitError{code: code}
	}
	return nil
}

func (a *app) runCommand() *cobra.Command {
	var lang string
	cmd := &cobra.Command{
		Use:   "run <box> --lang <python|node|bash> [file|-]",
		Short: "Run code on a box (from a file, or stdin with - or no file)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang == "" {
				return errors.New("--lang is required (python, node or bash)")
			}
			var code []byte
			var err error
			if len(args) == 2 && args[1] != "-" {
				code, err = os.ReadFile(args[1])
			} else {
				code, err = io.ReadAll(a.in)
			}
			if err != nil {
				return err
			}
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := box.RunCode(cmd.Context(), zipbox.Language(lang), string(code))
			if err != nil {
				return err
			}
			if err := a.emit(res, func() {
				io.WriteString(a.out, res.Stdout)
				io.WriteString(a.err, res.Stderr)
			}); err != nil {
				return err
			}
			if res.ExitCode != 0 {
				code := res.ExitCode
				if code < 0 {
					code = 1
				}
				return exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&lang, "lang", "", "python, node or bash")
	return cmd
}

func (a *app) filesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "files", Short: "Read and write files on a box (relative paths start at /root/workspace)"}
	withBox := func(use, short string, nargs int, run func(ctx context.Context, box *zipbox.Box, args []string) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.RangeArgs(nargs, nargs+1),
			RunE: func(cmd *cobra.Command, args []string) error {
				_, box, err := a.boxFor(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				return run(cmd.Context(), box, args[1:])
			},
		}
	}
	ls := withBox("ls <box> [path]", "List a directory", 1, func(ctx context.Context, box *zipbox.Box, args []string) error {
		dir := zipbox.WorkspaceDir
		if len(args) > 0 {
			dir = args[0]
		}
		entries, err := box.Files.List(ctx, dir)
		if err != nil {
			return err
		}
		if entries == nil {
			entries = []zipbox.FileEntry{}
		}
		return a.emit(entries, func() {
			w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
			for _, e := range entries {
				name := e.Name
				if e.Kind == zipbox.KindDirectory {
					name += "/"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\n", e.SizeBytes, e.ModifiedAt().Format(time.DateTime), name)
			}
			w.Flush()
		})
	})
	cat := withBox("cat <box> <path>", "Print a file", 2, func(ctx context.Context, box *zipbox.Box, args []string) error {
		if a.jsonOut {
			data, err := box.Files.Read(ctx, args[0])
			if err != nil {
				return err
			}
			return a.printJSON(map[string]string{"path": zipbox.ResolvePath(args[0]), "content": string(data)})
		}
		_, err := box.Files.ReadTo(ctx, args[0], a.out)
		return err
	})
	put := withBox("put <box> <local> <remote>", "Upload a file", 3, func(ctx context.Context, box *zipbox.Box, args []string) error {
		remote := args[1]
		if strings.HasSuffix(remote, "/") {
			remote += path.Base(args[0])
		}
		if args[0] == "-" {
			if err := box.Files.Write(ctx, remote, a.in); err != nil {
				return err
			}
		} else if err := box.Files.Upload(ctx, args[0], remote); err != nil {
			return err
		}
		return a.emit(map[string]string{"path": zipbox.ResolvePath(remote)}, func() {
			fmt.Fprintln(a.out, zipbox.ResolvePath(remote))
		})
	})
	get := withBox("get <box> <remote> [local]", "Download a file (local defaults to its name; - for stdout)", 2, func(ctx context.Context, box *zipbox.Box, args []string) error {
		local := path.Base(args[0])
		if len(args) > 1 {
			local = args[1]
		}
		if local == "-" {
			_, err := box.Files.ReadTo(ctx, args[0], a.out)
			return err
		}
		if err := box.Files.Download(ctx, args[0], local); err != nil {
			return err
		}
		return a.emit(map[string]string{"path": local}, func() { fmt.Fprintln(a.out, local) })
	})
	mutation := func(use, short string, nargs int, run func(ctx context.Context, box *zipbox.Box, args []string) error) *cobra.Command {
		c := withBox(use, short, nargs, func(ctx context.Context, box *zipbox.Box, args []string) error {
			if err := run(ctx, box, args); err != nil {
				return err
			}
			return a.emit(map[string]bool{"ok": true}, func() {})
		})
		c.Args = cobra.ExactArgs(nargs + 1)
		return c
	}
	rm := mutation("rm <box> <path>", "Delete a file or directory", 1, func(ctx context.Context, box *zipbox.Box, args []string) error {
		return box.Files.Delete(ctx, args[0])
	})
	mkdir := mutation("mkdir <box> <path>", "Create a directory", 1, func(ctx context.Context, box *zipbox.Box, args []string) error {
		return box.Files.Mkdir(ctx, args[0])
	})
	mv := mutation("mv <box> <src> <dst>", "Move or rename", 2, func(ctx context.Context, box *zipbox.Box, args []string) error {
		return box.Files.Move(ctx, args[0], args[1])
	})
	put.Args = cobra.ExactArgs(3)
	cat.Args = cobra.ExactArgs(2)
	cmd.AddCommand(ls, cat, put, get, rm, mkdir, mv)
	return cmd
}
