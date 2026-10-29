package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

// The Tailscale auth key is a credential for the box, sealed on this machine to
// the box's own key. It is read from stdin or a file and never from argv, where
// it would land in shell history and in every process listing.

const authKeyFlagHelp = "read the Tailscale auth key from this file (- for stdin)"

// readAuthKey reads the auth key from path, or from stdin when path is "" or "-".
// On a terminal it reads without echo. Errors never quote what was read.
func (a *app) readAuthKey(path string) (string, error) {
	var raw string
	switch {
	case path != "" && path != "-":
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the auth key file: %w", err)
		}
		raw = string(data)
	case a.stdinIsTerminal():
		f, ok := a.in.(*os.File)
		if !ok {
			return "", errors.New("pipe the Tailscale auth key on stdin, or pass --auth-key-file")
		}
		fmt.Fprint(a.err, "Paste your Tailscale auth key (tskey-…, input hidden): ")
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.err)
		if err != nil {
			return "", err
		}
		raw = string(data)
	default:
		line, err := bufio.NewReader(a.in).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		raw = line
	}
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errors.New("no Tailscale auth key: pipe it on stdin or pass --auth-key-file")
	}
	if !strings.HasPrefix(key, "tskey-") || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("that is not a Tailscale auth key (expected one line starting with tskey-); create a one-off key in the Tailscale admin console")
	}
	return key, nil
}

// connectTailscale seals the key to the box and sends it.
func (a *app) connectTailscale(ctx context.Context, box *zipbox.Box, key string) error {
	if err := box.Tailscale.Connect(ctx, key); err != nil {
		return err
	}
	return a.emit(map[string]any{"id": box.ID, "pending": true}, func() {
		fmt.Fprintf(a.out, "Auth key sealed to %s and sent. Check with `zipbox tailscale status %s`.\n", box.ID, box.ID)
	})
}

func (a *app) tailscaleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tailscale",
		Short: "Join a box to your own tailnet",
		Long: "Join a box to your tailnet with a one-off Tailscale auth key. The key is sealed on\n" +
			"this machine to the box's own key: Zipbox relays it once and never sees or stores it.",
	}
	var keyFile string
	connect := &cobra.Command{
		Use:   "connect <box>",
		Short: "Connect a box to your tailnet (auth key from stdin or --auth-key-file)",
		Example: "  zipbox tailscale connect demo < tskey.txt\n" +
			"  zipbox tailscale connect demo --auth-key-file tskey.txt",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := a.readAuthKey(keyFile)
			if err != nil {
				return err
			}
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.connectTailscale(cmd.Context(), box, key)
		},
	}
	connect.Flags().StringVar(&keyFile, "auth-key-file", "", authKeyFlagHelp)

	status := &cobra.Command{
		Use:   "status <box>",
		Short: "Show a box's tailnet status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			st, err := box.Tailscale.Status(cmd.Context())
			if err != nil {
				return err
			}
			return a.emit(map[string]any{"id": box.ID, "tailscale": st}, func() {
				if st == nil {
					fmt.Fprintf(a.out, "%s is not on a tailnet. Connect it with `zipbox tailscale connect %s`.\n", box.ID, box.ID)
					return
				}
				w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
				state := "unknown (box not running)"
				if st.State != nil {
					state = string(*st.State)
				}
				fmt.Fprintf(w, "state\t%s\n", state)
				fmt.Fprintf(w, "tailnet\t%s\n", deref(st.Tailnet))
				fmt.Fprintf(w, "ipv4\t%s\n", deref(st.IPv4))
				fmt.Fprintf(w, "ipv6\t%s\n", deref(st.IPv6))
				fmt.Fprintf(w, "dns name\t%s\n", deref(st.DNSName))
				if st.KeyExpiry != nil {
					fmt.Fprintf(w, "key expiry\t%s\n", st.KeyExpiry.Format("2006-01-02 15:04 MST"))
				}
				if st.Pending {
					fmt.Fprintln(w, "pending\tan auth key is on its way to the box")
				}
				w.Flush()
			})
		},
	}

	disconnect := &cobra.Command{
		Use:   "disconnect <box>",
		Short: "Sign a box out of its tailnet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, box, err := a.boxFor(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := box.Tailscale.Disconnect(cmd.Context()); err != nil {
				return err
			}
			return a.emit(map[string]string{"id": box.ID, "action": "tailscale disconnect"}, func() {
				fmt.Fprintf(a.out, "%s leaving its tailnet.\n", box.ID)
			})
		},
	}
	cmd.AddCommand(connect, status, disconnect)
	return cmd
}

func (a *app) privateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "private <on|off> <box>",
		Short: "Make a box private (tailnet only) or public again",
		Long: "A private box answers only devices on your tailnet: its URLs, previews, terminal,\n" +
			"IDE, desktop and SSH. Turning it on needs the box connected to your tailnet. The\n" +
			"Zipbox MCP server cannot reach a private box.",
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var enabled bool
			switch args[0] {
			case "on":
				enabled = true
			case "off":
				enabled = false
			default:
				return fmt.Errorf("want `zipbox private on <box>` or `zipbox private off <box>`, got %q", args[0])
			}
			_, box, err := a.boxFor(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			if err := box.SetPrivate(cmd.Context(), enabled); err != nil {
				return err
			}
			return a.emit(map[string]any{"id": box.ID, "private": enabled}, func() {
				if enabled {
					fmt.Fprintf(a.out, "%s turning private: only devices on your tailnet will reach it.\n", box.ID)
					return
				}
				fmt.Fprintf(a.out, "%s turning public.\n", box.ID)
			})
		},
	}
	return cmd
}
