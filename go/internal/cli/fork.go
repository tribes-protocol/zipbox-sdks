package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	zipbox "github.com/tribes-protocol/zipbox-go"
)

func (a *app) forkCommand() *cobra.Command {
	var name, tailscaleKeyFile string
	var pause, private, noWait bool
	cmd := &cobra.Command{
		Use:   "fork <source> --name <name>",
		Short: "Fork a box into a new box with the same files and a new identity",
		Long: "Fork a box: a new box with the source's files, installed software, settings and size,\n" +
			"a new name and a fresh identity. RAM and running processes are not copied.\n\n" +
			"A running source must be paused for a consistent capture: pass --pause to allow it (its\n" +
			"sessions disconnect and it resumes right after the capture). A paused source stays paused.\n\n" +
			"The new box and its operation are printed as soon as the fork is accepted. Waiting has no\n" +
			"deadline; Ctrl-C stops only the wait, the fork keeps running. Cancel the fork with\n" +
			"`zipbox sandbox cancel <name>`. The new name is kept even if the fork is cancelled.\n\n" +
			"The new box starts public, whatever the source's privacy, unless --private: then it is\n" +
			"reachable only from your tailnet. --tailscale-auth-key-file joins it to your\n" +
			"tailnet once it runs: the key is sealed on this machine to the new box's own key and is\n" +
			"never sent with the fork (https://zipbox.ai/docs/fork).",
		Example: "  zipbox fork demo --name demo-2\n" +
			"  zipbox fork demo --name demo-2 --pause          # demo is running: pause it for the capture\n" +
			"  zipbox fork build --name build-2 --private --tailscale-auth-key-file tskey.txt\n" +
			"  zipbox fork demo --name demo-2 --no-wait --json # print the accepted fork and return",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tailscaleKeyFile != "" && noWait {
				return errors.New("--tailscale-auth-key-file needs the wait: the key is sealed to the new box once it runs; drop --no-wait, or connect it later with `zipbox tailscale connect`")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			// Read the key before anything is sent, so a bad key never leaves a box behind.
			var tailscaleKey string
			if tailscaleKeyFile != "" {
				if tailscaleKey, err = a.readAuthKey(tailscaleKeyFile); err != nil {
					return err
				}
			}
			sourceID, err := resolveBox(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			policy := zipbox.ForkRequirePaused
			if pause {
				policy = zipbox.ForkPauseAndResume
			}
			fmt.Fprintf(a.err, "Forking %s into %s…\n", args[0], name)
			child, err := c.Sandboxes.Fork(cmd.Context(), sourceID, zipbox.ForkOptions{Name: name, SourcePolicy: policy, Private: private, NoWait: true})
			if err != nil {
				if zipbox.IsCode(err, zipbox.CodePauseRequired) && !a.jsonOut {
					fmt.Fprintf(a.err, "%s is running. Re-run with --pause to pause it for the capture: its sessions disconnect and it resumes right after.\n", args[0])
				}
				return err
			}
			operation := ""
			if child.Detail.Fork != nil {
				operation = child.Detail.Fork.OperationID
			}
			if noWait {
				return a.emit(child.Detail, func() { a.printDetail(child.Detail) })
			}
			fmt.Fprintf(a.err, "%s (%s) accepted, operation %s. Waiting until it is ready; Ctrl-C stops only this wait, the fork keeps running (cancel it with `zipbox sandbox cancel %s`).\n",
				name, child.ID, operation, name)
			ready, err := c.Sandboxes.WaitReady(cmd.Context(), child.ID)
			if err != nil {
				if !zipbox.IsCode(err, zipbox.CodeConflict) && !zipbox.IsCode(err, zipbox.CodeNotFound) {
					return err
				}
				// The new box ended or its fork removed it instead of becoming ready:
				// say why the fork ended.
				fork, forkErr := c.Sandboxes.GetFork(cmd.Context(), child.ID)
				if forkErr != nil {
					return err
				}
				if fork.Error != nil {
					return &zipbox.Error{Code: fork.Error.Code, Message: fork.Error.Message, DocsURL: "https://zipbox.ai/docs/errors#" + string(fork.Error.Code)}
				}
				if fork.Phase == zipbox.ForkCancelled {
					return fmt.Errorf("the fork into %s was cancelled; the name %s stays yours", name, name)
				}
				return err
			}
			if tailscaleKey != "" {
				if err := ready.Tailscale.Connect(cmd.Context(), tailscaleKey); err != nil {
					return err
				}
				fmt.Fprintf(a.err, "Tailscale auth key sealed to %s and sent. Check with `zipbox tailscale status %s`.\n", ready.ID, ready.ID)
			}
			return a.emit(ready.Detail, func() { a.printDetail(ready.Detail) })
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the new box's permanent subdomain (required)")
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().BoolVar(&pause, "pause", false, "allow pausing a running source for the capture (it resumes right after)")
	cmd.Flags().BoolVar(&private, "private", false, "start the new box private: reachable only from devices on your tailnet")
	cmd.Flags().StringVar(&tailscaleKeyFile, "tailscale-auth-key-file", "", "join your tailnet once the new box runs, with the auth key in this file (- for stdin)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "print the accepted fork and return without waiting")
	return cmd
}
